package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestHTTPProviderListsAndCallsToolsWithMeta(t *testing.T) {
	var got ProviderToolCallRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/tools":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tools": []Tool{{
					Name:        "search",
					Description: "Search",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				}},
			})
		case "/call":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode call request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(CallResult{
				Content: []ContentItem{{Type: "text", Text: "result"}},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	provider := HTTPProvider{BaseURL: server.URL, Token: "token", Client: server.Client()}
	list, err := provider.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(list) != 1 || list[0].Name != "search" {
		t.Fatalf("unexpected tools: %#v", list)
	}
	result, err := provider.CallTool("search", json.RawMessage(`{"query":"x"}`), tools.CommandExecutionContext{
		AppID:           "app-a",
		RunID:           "run-1",
		AgentID:         "agent-1",
		ExternalActorID: "user-1",
		WorkspaceID:     "ws-1",
		Target:          agentcore.TargetRef{Type: "workspace", ID: "ws-1"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "result" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if got.ToolName != "search" || string(got.Input) != `{"query":"x"}` {
		t.Fatalf("unexpected call payload: %#v", got)
	}
	if got.Meta.AppID != "app-a" ||
		got.Meta.RunID != "run-1" ||
		got.Meta.AgentID != "agent-1" ||
		got.Meta.ExternalActorID != "user-1" ||
		got.Meta.WorkspaceID != "ws-1" ||
		got.Meta.Target.Type != "workspace" ||
		got.Meta.Target.ID != "ws-1" {
		t.Fatalf("unexpected meta: %#v", got.Meta)
	}
}
