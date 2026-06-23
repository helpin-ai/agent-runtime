package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestHTTPProviderLifecycle(t *testing.T) {
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/prepare":
			var req PrepareRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode prepare: %v", err)
			}
			if req.RunID != "run-1" || req.Target.ID != "repo-1" {
				t.Fatalf("unexpected prepare request: %#v", req)
			}
			_ = json.NewEncoder(w).Encode(agentcore.WorkspaceLease{
				ID:       "lease-1",
				Provider: "host_app",
				RootPath: "/tmp/repo",
			})
		case "/finalize":
			var req FinalizeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode finalize: %v", err)
			}
			if req.Outcome != agentcore.RunStatusCompleted || req.Lease.ID != "lease-1" {
				t.Fatalf("unexpected finalize request: %#v", req)
			}
			_ = json.NewEncoder(w).Encode(FinalizeResult{})
		case "/cleanup":
			var req CleanupRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode cleanup: %v", err)
			}
			if req.Reason != "completed" || req.Lease.ID != "lease-1" {
				t.Fatalf("unexpected cleanup request: %#v", req)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := HTTPProvider{BaseURL: server.URL, Token: "token"}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID: "app-a",
		RunID: "run-1",
		Target: agentcore.TargetRef{
			Type: "repository",
			ID:   "repo-1",
		},
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if lease.CleanupPolicy != CleanupOnTerminal {
		t.Fatalf("cleanup policy = %q", lease.CleanupPolicy)
	}
	if _, err := provider.FinalizeWorkspace(context.Background(), FinalizeRequest{
		AppID:   "app-a",
		RunID:   "run-1",
		Target:  agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:   *lease,
		Outcome: agentcore.RunStatusCompleted,
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if err := provider.CleanupWorkspace(context.Background(), CleanupRequest{
		AppID:  "app-a",
		RunID:  "run-1",
		Target: agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Lease:  *lease,
		Reason: "completed",
	}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("expected three lifecycle calls, got %v", paths)
	}
}

func TestShouldCleanup(t *testing.T) {
	if !ShouldCleanup(&agentcore.WorkspaceLease{CleanupPolicy: CleanupAlways}, false) {
		t.Fatal("always should cleanup for non-terminal outcomes")
	}
	if ShouldCleanup(&agentcore.WorkspaceLease{CleanupPolicy: CleanupManual}, true) {
		t.Fatal("manual should not auto cleanup")
	}
	if !ShouldCleanup(&agentcore.WorkspaceLease{CleanupPolicy: CleanupOnTerminal}, true) {
		t.Fatal("on_terminal should cleanup terminal outcomes")
	}
	if ShouldCleanup(&agentcore.WorkspaceLease{CleanupPolicy: CleanupOnTerminal}, false) {
		t.Fatal("on_terminal should not cleanup non-terminal outcomes")
	}
}
