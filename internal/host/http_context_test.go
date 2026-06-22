package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestHTTPContextProviderSendsRunScopedRequest(t *testing.T) {
	var got TargetContextRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token-1" {
			t.Fatalf("unexpected authorization header: %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(TargetContext{
			Target:  got.Target,
			Summary: "fresh context",
			Data:    map[string]interface{}{"source": "test"},
		})
	}))
	defer server.Close()

	provider := HTTPContextProvider{Endpoint: server.URL, Token: "token-1", Client: server.Client()}
	ctx, err := provider.ResolveRunTarget(context.Background(), TargetContextRequest{
		AppID:   "app-a",
		RunID:   "run-1",
		AgentID: "agent-1",
		Target:  agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Trigger: map[string]interface{}{"kind": "manual"},
	})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	if got.RunID != "run-1" || got.AgentID != "agent-1" || got.Trigger["kind"] != "manual" {
		t.Fatalf("request did not include run context: %#v", got)
	}
	if ctx.Summary != "fresh context" {
		t.Fatalf("unexpected summary: %q", ctx.Summary)
	}
}
