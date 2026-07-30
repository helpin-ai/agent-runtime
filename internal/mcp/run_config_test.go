package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestPrepareStoredServersEncryptsCredential(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	expires := time.Now().Add(time.Hour).UTC()
	servers, err := PrepareStoredServers("app-1", "run-1", []RunServerRequest{{
		ServerID: "workspace-server-1", ServerName: "github", Transport: agentcore.MCPTransportStreamableHTTP,
		URL: "https://mcp.example.com/mcp", Tools: []RunTool{{Name: "get_issue", Access: agentcore.MCPToolAccessRead}},
		Credential: &RunCredential{Type: CredentialBearerToken, AccessToken: "super-secret-token", ExpiresAt: &expires},
	}}, RunConfig{CredentialKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || len(servers[0].EncryptedCredential) == 0 {
		t.Fatalf("expected one encrypted server, got %#v", servers)
	}
	if strings.Contains(string(servers[0].EncryptedCredential), "super-secret-token") {
		t.Fatal("encrypted credential contains plaintext token")
	}
	credential, err := decryptCredential(key, servers[0])
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "super-secret-token" {
		t.Fatalf("access token = %q", credential.AccessToken)
	}
}

func TestPrepareStoredServersRejectsUnsafeOrAmbiguousPolicy(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	tests := []struct {
		name string
		req  RunServerRequest
		want string
	}{
		{name: "http", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "http://example.com/mcp", Tools: []RunTool{{Name: "read", Access: "read"}}}, want: "must use https"},
		{name: "no tools", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://example.com/mcp"}, want: "at least one"},
		{name: "unknown access", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://example.com/mcp", Tools: []RunTool{{Name: "read", Access: "maybe"}}}, want: "read or write"},
		{name: "reserved header", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://example.com/mcp", Tools: []RunTool{{Name: "read", Access: "read"}}, Credential: &RunCredential{Type: CredentialHeaders, Headers: map[string]string{"Host": "evil"}}}, want: "reserved"},
		{name: "invalid header name", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://example.com/mcp", Tools: []RunTool{{Name: "read", Access: "read"}}, Credential: &RunCredential{Type: CredentialHeaders, Headers: map[string]string{"Bad Header": "secret"}}}, want: "credential headers"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := PrepareStoredServers("app", "run", []RunServerRequest{test.req}, RunConfig{CredentialKey: key})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSafeRunHTTPClientDisablesProcessProxy(t *testing.T) {
	client := safeRunHTTPClient("https://mcp.example.com/mcp", nil, RunConfig{})
	bounded, ok := client.Transport.(boundedResponseRoundTripper)
	if !ok {
		t.Fatalf("unexpected outer transport %T", client.Transport)
	}
	withHeaders, ok := bounded.base.(headerRoundTripper)
	if !ok {
		t.Fatalf("unexpected credential transport %T", bounded.base)
	}
	transport, ok := withHeaders.base.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected HTTP transport %T", withHeaders.base)
	}
	if transport.Proxy != nil {
		t.Fatal("run MCP transport must not use a process-level HTTP proxy")
	}
}

func TestPrepareRunToolsConnectsAndAuditsRemoteTool(t *testing.T) {
	remote := protocol.NewServer(&protocol.Implementation{Name: "test-mcp", Version: "1.0.0"}, nil)
	remote.AddTool(&protocol.Tool{
		Name: "get_issue", Description: "Get an issue",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false},
	}, func(_ context.Context, req *protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		var input map[string]any
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return nil, err
		}
		return &protocol.CallToolResult{Content: []protocol.Content{&protocol.TextContent{Text: "issue:" + input["id"].(string)}}}, nil
	})
	createCalled := false
	remote.AddTool(&protocol.Tool{
		Name: "create_issue", Description: "Create an issue",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
	}, func(_ context.Context, _ *protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		createCalled = true
		return &protocol.CallToolResult{Content: []protocol.Content{&protocol.TextContent{Text: "created"}}}, nil
	})
	handler := protocol.NewStreamableHTTPHandler(func(*http.Request) *protocol.Server { return remote }, &protocol.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer run-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, req)
	}))
	defer server.Close()

	key := []byte("0123456789abcdef0123456789abcdef")
	cfg := RunConfig{CredentialKey: key, AllowHTTP: true, AllowPrivateNetwork: true, ConnectTimeout: time.Second}
	stored, err := PrepareStoredServers("app", "run", []RunServerRequest{{
		ServerID: "mcp-1", ServerName: "github", Transport: agentcore.MCPTransportStreamableHTTP, URL: server.URL,
		Tools: []RunTool{
			{Name: "get_issue", Access: agentcore.MCPToolAccessRead},
			{Name: "create_issue", Access: agentcore.MCPToolAccessWrite},
		},
		Credential: &RunCredential{Type: CredentialBearerToken, AccessToken: "run-token"},
	}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	agent := &agentcore.Agent{ID: "agent", AppID: "app", Name: "Agent", RuntimeKind: agentcore.RuntimeNativeSDK, ApprovalMode: agentcore.ApprovalModeMutatingTools}
	if err := memory.CreateAgent(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{ID: "run", AppID: "app", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "workspace", ID: "ws"}, Status: agentcore.RunStatusRunning}
	if err := memory.CreateRunWithMCP(context.Background(), run, stored); err != nil {
		t.Fatal(err)
	}
	registry, allowed, closeTools, err := PrepareRunTools(context.Background(), memory, tools.NewRegistry(), "app", "run", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTools()
	alias := "mcp__github__get_issue"
	if !allowed[alias] {
		t.Fatalf("allowed = %#v", allowed)
	}
	result, err := NewGatewayWithAllowed(memory, registry, allowed).CallTool(context.Background(), "app", "run", ToolCallRequest{ToolName: alias, Input: json.RawMessage(`{"id":"42"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "issue:42") {
		t.Fatalf("result = %#v", result)
	}
	calls, err := memory.ListToolCalls(context.Background(), "app", "run")
	if err != nil || len(calls) != 1 || calls[0].ToolName != alias {
		t.Fatalf("calls = %#v, err = %v", calls, err)
	}
	writeAlias := "mcp__github__create_issue"
	approval, err := NewGatewayWithAllowed(memory, registry, allowed).CallTool(context.Background(), "app", "run", ToolCallRequest{ToolName: writeAlias, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !approval.ApprovalRequired || createCalled {
		t.Fatalf("expected approval before remote mutation, result=%#v called=%v", approval, createCalled)
	}
}
