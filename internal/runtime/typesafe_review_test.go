package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTypeSafeApprovalRouting(t *testing.T) {
	for _, tt := range []struct {
		name, mode, choice             string
		confidence                     float64
		allowed, malformed, wantPrompt bool
	}{
		{"authorized push", agentcore.ApprovalModeRiskBased, "approve", 0.99, true, false, false},
		{"uncertain", agentcore.ApprovalModeRiskBased, "approve", 0.94, true, false, true},
		{"destructive", agentcore.ApprovalModeRiskBased, "human", 0.99, true, false, true},
		{"human only", agentcore.ApprovalModeAlways, "approve", 1, true, false, true},
		{"never", agentcore.ApprovalModeNever, "human", 1, true, false, false},
		{"malformed", agentcore.ApprovalModeRiskBased, "approve", 1, true, true, true},
		{"hard denial", agentcore.ApprovalModeRiskBased, "approve", 1, false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			x := contextTestExec(t)
			x.Run.ExternalActorID = "user"
			root := t.TempDir()
			for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "--allow-empty", "-m", "initial"}, {"checkout", "-b", "fix"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git fixture: %s %v", output, err)
				}
			}
			x.WorkspaceLease = &agentcore.WorkspaceLease{RootPath: root, Provider: "repository", Metadata: map[string]any{"base_branch": "main", "repository_id": "repo"}}
			x.Run.WorkspaceLease = x.WorkspaceLease
			x.WorkspaceLease = &agentcore.WorkspaceLease{Provider: "analysis", RootPath: t.TempDir()} // superseded scratch

			x.Agent.ApprovalMode = tt.mode
			x.AllowedTools = map[string]bool{"commit_and_push": tt.allowed}
			calls, reviews := 0, 0
			x.Tools.Register(tools.Definition{Name: "commit_and_push", Mutating: true}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
				calls++
				return json.RawMessage(`{}`), nil
			})
			r, err := openNativeRecorder(x.Context, x, false)
			if err != nil {
				t.Fatal(err)
			}
			result := &nativeExecutionResult{Messages: []NativeMessage{{Role: "user", Provenance: "human", Content: "Push the tested fix to my feature branch."}}}
			if err := r.save(x.Context, "tools", result); err != nil {
				t.Fatal(err)
			}
			reviewer := &TypeSafeReviewer{APIKey: "review-key", Model: "jev-latest", Threshold: .95, AutoApprove: true, Client: &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
				reviews++
				if req.URL.String() != typeSafeEndpoint {
					t.Fatal("agent endpoint inherited")
				}
				body, _ := json.Marshal(map[string]any{"model": "jev-latest", "answers": map[string]any{"decision": map[string]any{"type": "choice", "choice": tt.choice, "confidence": tt.confidence}}, "usage": map[string]int{"input_tokens": 100, "output_tokens": 2}})
				if tt.malformed {
					body = []byte(`{"answers":null}`)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
			})}}
			ctx := context.WithValue(x.Context, nativeCallRecorderKey{}, &nativeCallRecorder{recorder: r, result: result})
			ctx = context.WithValue(ctx, typeSafeReviewKey{}, reviewer)
			got := executeSingleNativeToolCall(ctx, x, NativeBlock{ToolName: "commit_and_push", ToolCallID: "push", Input: json.RawMessage(`{"message":"fix"}`)})
			if tt.wantPrompt != got.ApprovalRequired {
				t.Fatalf("prompt=%v output=%s", got.ApprovalRequired, got.Output)
			}
			if !tt.allowed {
				if calls != 0 || reviews != 0 || !got.IsError {
					t.Fatal("hard denial bypassed")
				}
				return
			}
			if tt.wantPrompt && calls != 0 {
				t.Fatal("prompted action executed")
			}
			if tt.mode != agentcore.ApprovalModeRiskBased && reviews != 0 {
				t.Fatal("human/never mode reviewed")
			}
			if result.Usage.InputTokens != 0 {
				t.Fatal("review billed to parent model")
			}
		})
	}
}

func TestTypeSafeMissingKeyAndCredentialRedaction(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("AGENT_RUNTIME_TYPESAFE_ENABLED", "true")
	if typeSafeReviewerFromEnv() != nil {
		t.Fatal("enabled without key")
	}
	t.Setenv("DATABASE_URL", "postgres://private-password@db/internal")
	if strings.Contains(redactReviewCredentials("connect postgres://private-password@db/internal"), "private-password") {
		t.Fatal("credential leaked")
	}
}

func TestTypeSafeProviderFailuresPreserveExistingPolicy(t *testing.T) {
	for _, failure := range []string{"timeout", "api_error", "invalid_json"} {
		t.Run(failure, func(t *testing.T) {
			x := contextTestExec(t)
			x.Run.ExternalActorID = "owner"
			x.Agent.ApprovalMode = agentcore.ApprovalModeRiskBased
			r, err := openNativeRecorder(x.Context, x, false)
			if err != nil {
				t.Fatal(err)
			}
			result := &nativeExecutionResult{Messages: []NativeMessage{{Role: "user", Provenance: "human", Content: "Run the tests"}}}
			reviewer := &TypeSafeReviewer{APIKey: "key", Threshold: .95, AutoApprove: true, Client: &http.Client{Transport: reviewTransport(func(*http.Request) (*http.Response, error) {
				if failure == "timeout" {
					return nil, context.DeadlineExceeded
				}
				status := 200
				if failure == "api_error" {
					status = 503
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("invalid")), Header: make(http.Header)}, nil
			})}}
			ctx := context.WithValue(x.Context, nativeCallRecorderKey{}, &nativeCallRecorder{recorder: r, result: result})
			ctx = context.WithValue(ctx, typeSafeReviewKey{}, reviewer)
			for _, existing := range []bool{true, false} {
				if got := nativeReviewApproval(ctx, x, tools.Definition{Name: "run_command", Mutating: true}, NativeBlock{ToolName: "run_command", Input: json.RawMessage(`{"program":"go","args":["test","./..."]}`)}, existing); got != existing {
					t.Fatalf("failure changed policy: got %v want %v", got, existing)
				}
			}
		})
	}
}

func TestReviewRedactsJSONEscapedCredentials(t *testing.T) {
	secret := "postgres://db/secret?sslmode=require&key=private"
	t.Setenv("DATABASE_URL", secret)
	encoded, _ := json.Marshal(map[string]string{"source": secret})
	if got := redactReviewCredentials(string(encoded)); strings.Contains(got, "private") {
		t.Fatal("escaped credential leaked")
	}
}
