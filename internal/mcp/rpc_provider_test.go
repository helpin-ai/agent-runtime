package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	runtimetools "github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestStreamableHTTPProviderListsAndCallsTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		switch req.Method {
		case "tools/list":
			_ = json.NewEncoder(w).Encode(rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"tools":[{"name":"search","description":"Search","inputSchema":{"type":"object"}}]}`),
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"content":[{"type":"text","text":"result"}]}`),
			})
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	provider := &StreamableHTTPProvider{Endpoint: server.URL, Token: "token", Client: server.Client()}
	tools, err := provider.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "search" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	result, err := provider.CallTool(context.Background(), "search", json.RawMessage(`{"query":"x"}`), runtimetools.CommandExecutionContext{})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "result" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

// A tools/call names the calling run and the model's tool call in _meta,
// so a server can tell a retried call from a new one.
func TestToolCallCarriesRunAndToolCallIDs(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got, _ = req.Params["_meta"].(map[string]any)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer server.Close()
	provider := &StreamableHTTPProvider{Endpoint: server.URL, Client: server.Client()}
	ctx := runtimetools.WithToolCallID(context.Background(), "call-7")
	if _, err := provider.CallTool(ctx, "start", json.RawMessage(`{}`), runtimetools.CommandExecutionContext{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	if got[MetaRunID] != "run-1" || got[MetaToolCallID] != "call-7" {
		t.Fatalf("_meta = %v", got)
	}
}
