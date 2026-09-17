package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
		{"authorized push", agentcore.ApprovalModeRiskBased, "approve", 0.99, true, false, true},
		{"local tests", agentcore.ApprovalModeRiskBased, "approve", 0.99, true, false, false},
		{"routine hazard", agentcore.ApprovalModeRiskBased, "human", .99, true, false, true},
		{"routine normal", agentcore.ApprovalModeRiskBased, "approve", .99, true, false, false},
		{"uncertain", agentcore.ApprovalModeRiskBased, "approve", 0.89, true, false, true},
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
			tool := "run_command"
			if tt.name == "authorized push" {
				tool = "commit_and_push"
			}
			x.AllowedTools = map[string]bool{tool: tt.allowed}
			calls, reviews := 0, 0
			definition := tools.Definition{Name: tool, Mutating: true}
			if strings.HasPrefix(tt.name, "routine") {
				definition.RiskLevel = tools.RiskLevelRoutine
			}
			x.Tools.Register(definition, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
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
			reviewer := &TypeSafeReviewer{APIKey: "review-key", Model: "jev-latest", EvaluatedModel: "jev-1.13.0", AskedThreshold: .90, HazardThreshold: .20, EscalationThreshold: .50, AutoApprove: true, Client: &http.Client{Transport: reviewTransport(func(req *http.Request) (*http.Response, error) {
				reviews++
				if req.URL.String() != typeSafeEndpoint {
					t.Fatal("agent endpoint inherited")
				}
				var request struct {
					Questions map[string]struct {
						Type string `json:"type"`
					} `json:"questions"`
				}
				if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if len(request.Questions) != 7 {
					t.Fatal("expected seven atomic questions")
				}
				for _, q := range request.Questions {
					if q.Type != "noul" {
						t.Fatal("expected noul question")
					}
				}
				response := testTypeSafeResult(tt.confidence, .10)
				if tt.choice == "human" {
					value := .90
					response.Answers["irreversible_delete"] = typeSafeAnswer{Type: "noul", Noul: &value}
				}
				body, _ := json.Marshal(response)
				if tt.malformed {
					body = []byte(`{"answers":null}`)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
			})}}
			ctx := context.WithValue(x.Context, nativeCallRecorderKey{}, &nativeCallRecorder{recorder: r, result: result})
			ctx = context.WithValue(ctx, typeSafeReviewKey{}, reviewer)
			got := executeSingleNativeToolCall(ctx, x, NativeBlock{ToolName: tool, ToolCallID: "call", Input: json.RawMessage(`{"message":"fix", "program":"pytest", "args":["-q"]}`)})
			if tt.wantPrompt != got.ApprovalRequired {
				t.Fatalf("prompt=%v output=%s", got.ApprovalRequired, got.Output)
			}
			if !tt.allowed {
				if calls != 0 || reviews != 0 || !got.IsError {
					t.Fatal("hard denial bypassed")
				}
				return
			}
			if !tt.wantPrompt && calls != 1 {
				t.Fatalf("expected tool execution: %s", got.Output)
			}
			if tt.name == "routine hazard" && !strings.Contains(got.Output, "irreversible_delete=0.90") {
				t.Fatal("approval card missing hazard scores")
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
			reviewer := &TypeSafeReviewer{APIKey: "key", AskedThreshold: .90, HazardThreshold: .20, EscalationThreshold: .50, AutoApprove: true, Client: &http.Client{Transport: reviewTransport(func(*http.Request) (*http.Response, error) {
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

func testTypeSafeResult(asked, hazard float64) *typeSafeResult {
	answers := map[string]typeSafeAnswer{}
	for name := range typeSafeQuestions {
		v := hazard
		answers[name] = typeSafeAnswer{Type: "noul", Noul: &v}
	}
	answers["user_asked"] = typeSafeAnswer{Type: "noul", Noul: &asked}
	return &typeSafeResult{Model: "jev-1.13.0", Answers: answers, Usage: json.RawMessage(`{"input_tokens":600,"output_tokens":7}`)}
}

func TestTypeSafeAtomicPolicy(t *testing.T) {
	reviewer := &TypeSafeReviewer{EvaluatedModel: "jev-1.13.0", AskedThreshold: .90, HazardThreshold: .20, EscalationThreshold: .50, AutoApprove: true}
	local := NativeBlock{ToolName: "run_command", Input: json.RawMessage(`{"program":"pytest","args":["-q"]}`)}
	for _, tt := range []struct {
		name           string
		asked, hazard  float64
		baseline, want bool
	}{
		{"local boundary", .90, .20, true, false},
		{"uncertain intent", .89, .20, true, true},
		{"hazard over limit", .99, .21, true, true},
		{"routine baseline", .1, .30, false, false},
		{"routine escalation", .99, .51, false, true},
		{"escalation is strict", .99, .50, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewer.policy(testTypeSafeResult(tt.asked, tt.hazard), local, tt.baseline); got != tt.want {
				t.Fatalf("prompt=%v want %v", got, tt.want)
			}
		})
	}
	// No amount of reviewer confidence grants publication or install permission.
	for _, call := range []NativeBlock{
		{ToolName: "commit_and_push", Input: json.RawMessage(`{}`)},
		{ToolName: "open_pr", Input: json.RawMessage(`{}`)},
		{ToolName: "run_command", Input: json.RawMessage(`{"program":"git","args":["push","origin","HEAD"]}`)},
		{ToolName: "run_command", Input: json.RawMessage(`{"program":"pip","args":["install","reqeusts"]}`)},
		{ToolName: "run_command", Input: json.RawMessage(`{"program":"python3","args":["-m","pip","install","pandas==2.2.2"]}`)},
		{ToolName: "run_command", Input: json.RawMessage(`{"program":"curl","args":["https://example.test"]}`)},
		{ToolName: "run_command", Input: json.RawMessage(`{"program":"rm","args":["-rf","/tmp/agent-runtime-workspaces"]}`)},
	} {
		result := testTypeSafeResult(1, 0)
		value := 1.0
		result.Answers["external_send_requested"] = typeSafeAnswer{Type: "noul", Noul: &value}
		if !reviewer.policy(result, call, true) {
			t.Fatalf("external/unsafe operation approved: %+v", call)
		}
	}
	result := testTypeSafeResult(1, 0)
	result.Model = "jev-1.14.0"
	if !reviewer.policy(result, local, true) {
		t.Fatal("unevaluated version auto-approved")
	}
	reviewer.AutoApprove = false
	if !reviewer.policy(testTypeSafeResult(1, 0), local, true) {
		t.Fatal("auto-approved without opt-in")
	}
	if reviewer.policy(testTypeSafeResult(.95, .1), local, false) {
		t.Fatal("observe mode broadened ordinary prompt policy")
	}
	if !reviewer.policy(testTypeSafeResult(.95, .6), local, false) {
		t.Fatal("observe mode failed hazard escalation")
	}
}

func TestTypeSafeProbabilityValidation(t *testing.T) {
	for _, kind := range []string{"missing", "null", "out_of_range", "negative", "wrong_type", "no_model"} {
		t.Run(kind, func(t *testing.T) {
			result := testTypeSafeResult(.95, .1)
			switch kind {
			case "missing":
				delete(result.Answers, "external_send")
			case "null":
				result.Answers["external_send"] = typeSafeAnswer{Type: "noul"}
			case "out_of_range":
				v := 695.0
				result.Answers["external_send"] = typeSafeAnswer{Type: "noul", Noul: &v}
			case "negative":
				v := -.1
				result.Answers["external_send"] = typeSafeAnswer{Type: "noul", Noul: &v}
			case "wrong_type":
				v := .1
				result.Answers["external_send"] = typeSafeAnswer{Type: "choice", Noul: &v}
			case "no_model":
				result.Model = ""
			}
			body, _ := json.Marshal(result)
			r := &TypeSafeReviewer{Client: &http.Client{Transport: reviewTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}}
			if _, err := r.review(context.Background(), map[string]any{}); err == nil {
				t.Fatal("malformed probability accepted")
			}
		})
	}
}

func TestTypeSafeOperatorThresholds(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	t.Setenv("AGENT_RUNTIME_TYPESAFE_ENABLED", "true")
	t.Setenv("AGENT_RUNTIME_TYPESAFE_AUTO_APPROVE", "true")
	t.Setenv("AGENT_RUNTIME_TYPESAFE_THRESHOLD", "")
	for _, name := range []string{"ASKED_THRESHOLD", "HAZARD_THRESHOLD", "ESCALATION_THRESHOLD", "MODEL", "EVALUATED_MODEL"} {
		t.Setenv("AGENT_RUNTIME_TYPESAFE_"+name, "")
	}
	r := typeSafeReviewerFromEnv()
	if r == nil || r.AskedThreshold != .90 || r.HazardThreshold != .20 || r.EscalationThreshold != .50 || r.EvaluatedModel != "jev-1.13.0" {
		t.Fatalf("wrong defaults: %+v", r)
	}
	t.Setenv("AGENT_RUNTIME_TYPESAFE_ASKED_THRESHOLD", "0.93")
	if typeSafeReviewerFromEnv().AskedThreshold != .93 {
		t.Fatal("operator threshold ignored")
	}
	t.Setenv("AGENT_RUNTIME_TYPESAFE_THRESHOLD", "0.95")
	if typeSafeReviewerFromEnv().AutoApprove {
		t.Fatal("legacy opt-in reused")
	}
	for _, bad := range []string{"NaN", "Inf", "-0.1", "1.1", "invalid"} {
		t.Setenv("AGENT_RUNTIME_TYPESAFE_HAZARD_THRESHOLD", bad)
		if typeSafeReviewerFromEnv() != nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestTypeSafeContextChangeRequiresHuman(t *testing.T) {
	x := contextTestExec(t)
	x.Run.ExternalActorID = "owner"
	x.Agent.ApprovalMode = agentcore.ApprovalModeRiskBased
	root := t.TempDir()
	path := filepath.Join(root, "test_example.py")
	if err := os.WriteFile(path, []byte("assert 1 == 1"), 0600); err != nil {
		t.Fatal(err)
	}
	x.Run.WorkspaceLease = &agentcore.WorkspaceLease{RootPath: root}
	recorder, err := openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	result := &nativeExecutionResult{Messages: []NativeMessage{{Role: "user", Provenance: "human", Content: "Run this test."}}}
	reviewed := 0
	reviewer := &TypeSafeReviewer{AskedThreshold: .90, HazardThreshold: .20, EscalationThreshold: .50, EvaluatedModel: "jev-1.13.0", AutoApprove: true, Client: &http.Client{Transport: reviewTransport(func(*http.Request) (*http.Response, error) {
		reviewed++
		if err := os.WriteFile(path, []byte("print('changed after review')"), 0600); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(testTypeSafeResult(.99, .1))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}}
	ctx := context.WithValue(x.Context, nativeCallRecorderKey{}, &nativeCallRecorder{recorder: recorder, result: result})
	ctx = context.WithValue(ctx, typeSafeReviewKey{}, reviewer)
	call := NativeBlock{ToolName: "run_command", ToolCallID: "changed", Input: json.RawMessage(`{"program":"pytest","args":["test_example.py"]}`)}
	if !nativeReviewApproval(ctx, x, tools.Definition{Name: "run_command", Mutating: true}, call, true) || reviewed != 1 {
		t.Fatal("changed script reused approval")
	}
}

func TestTypeSafeContextUsesLatestTrustedUserTurn(t *testing.T) {
	x := contextTestExec(t)
	x.Run.ExternalActorID = "owner"
	messages := []NativeMessage{
		{Role: "user", Provenance: "human", Content: "Earlier unrelated analysis request."},
		{Role: "assistant", Content: "Earlier response."},
		{Role: "user", Provenance: "human", Content: "Run exactly ls -la.\n<page_context>{\"untrusted\":true}</page_context>"},
		{Role: "user", Provenance: "human", Content: "The paused run was resumed with intent \"approve\".\n\nResume message (host-supplied context):\nApproved. Continue."},
	}
	state, err := typeSafeContext(x, messages, NativeBlock{ToolName: "run_command", ToolCallID: "local", Input: json.RawMessage(`{"program":"ls","args":["-la"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	trusted, ok := state["trusted_user_message"].(string)
	if !ok || trusted != "Run exactly ls -la.\n" {
		t.Fatalf("trusted message=%#v", state["trusted_user_message"])
	}
	if len(state) != 4 {
		t.Fatalf("review state keys=%v, want the evaluated four-field shape", state)
	}
	operation, ok := state["proposed_operation"].(map[string]any)
	if !ok || operation["tool_name"] != "run_command" {
		t.Fatalf("proposed operation=%#v", state["proposed_operation"])
	}
}
