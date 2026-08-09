package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
		Skills:     []agentcore.SkillRef{{Key: "github_triage"}},
		Credential: &RunCredential{Type: CredentialBearerToken, AccessToken: "super-secret-token", ExpiresAt: &expires},
	}}, RunConfig{CredentialKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || len(servers[0].EncryptedCredential) == 0 {
		t.Fatalf("expected one encrypted server, got %#v", servers)
	}
	if len(servers[0].Skills) != 1 || servers[0].Skills[0].Key != "github_triage" {
		t.Fatalf("connector skills were not persisted: %#v", servers[0].Skills)
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
		{name: "reserved cookie", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://example.com/mcp", Tools: []RunTool{{Name: "read", Access: "read"}}, Credential: &RunCredential{Type: CredentialHeaders, Headers: map[string]string{"Cookie": "session=evil"}}}, want: "reserved"},
		{name: "duplicate canonical header", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://example.com/mcp", Tools: []RunTool{{Name: "read", Access: "read"}}, Credential: &RunCredential{Type: CredentialHeaders, Headers: map[string]string{"x-api-key": "one", "X-Api-Key": "two"}}}, want: "duplicated"},
		{name: "invalid header name", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://example.com/mcp", Tools: []RunTool{{Name: "read", Access: "read"}}, Credential: &RunCredential{Type: CredentialHeaders, Headers: map[string]string{"Bad Header": "secret"}}}, want: "credential headers"},
		{name: "empty skill", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://example.com/mcp", Tools: []RunTool{{Name: "read", Access: "read"}}, Skills: []agentcore.SkillRef{{}}}, want: "requires skill_id or key"},
		{name: "duplicate skill", req: RunServerRequest{ServerID: "s", ServerName: "n", Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://example.com/mcp", Tools: []RunTool{{Name: "read", Access: "read"}}, Skills: []agentcore.SkillRef{{Key: "routing"}, {Key: "routing"}}}, want: "duplicates skill reference"},
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
	withStatus, ok := bounded.base.(authenticationStatusRoundTripper)
	if !ok {
		t.Fatalf("unexpected credential transport %T", bounded.base)
	}
	withHeaders, ok := withStatus.base.(headerRoundTripper)
	if !ok {
		t.Fatalf("unexpected header transport %T", withStatus.base)
	}
	transport, ok := withHeaders.base.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected HTTP transport %T", withHeaders.base)
	}
	if transport.Proxy != nil {
		t.Fatal("run MCP transport must not use a process-level HTTP proxy")
	}
}

func TestPrivateAddressIncludesSharedCarrierSpace(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "100.64.0.1", "224.0.0.1"} {
		if !isPrivateAddress(net.ParseIP(value)) {
			t.Fatalf("expected %s to be rejected as non-public", value)
		}
	}
	if isPrivateAddress(net.ParseIP("8.8.8.8")) {
		t.Fatal("expected public address to remain allowed")
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
	registry, allowed, _, closeTools, err := PrepareRunTools(context.Background(), memory, tools.NewRegistry(), "app", "run", cfg)
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

func TestPrepareRunToolsRecordsUnauthorizedDuringToolCall(t *testing.T) {
	remote := protocol.NewServer(&protocol.Implementation{Name: "test-mcp", Version: "1.0.0"}, nil)
	remote.AddTool(&protocol.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, _ *protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		return &protocol.CallToolResult{Content: []protocol.Content{&protocol.TextContent{Text: "ok"}}}, nil
	})
	handler := protocol.NewStreamableHTTPHandler(func(*http.Request) *protocol.Server { return remote }, &protocol.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	var authorized atomic.Bool
	authorized.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !authorized.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, req)
	}))
	defer server.Close()

	key := []byte("0123456789abcdef0123456789abcdef")
	cfg := RunConfig{CredentialKey: key, AllowHTTP: true, AllowPrivateNetwork: true, ConnectTimeout: time.Second}
	stored, err := PrepareStoredServers("app", "run-auth", []RunServerRequest{{
		ServerID: "mcp-1", ServerName: "customer_io", Transport: agentcore.MCPTransportStreamableHTTP,
		URL: server.URL, Tools: []RunTool{{Name: "read", Access: agentcore.MCPToolAccessRead}},
		Credential: &RunCredential{Type: CredentialBearerToken, AccessToken: "run-token"},
	}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	if err := memory.CreateAgent(context.Background(), &agentcore.Agent{ID: "agent", AppID: "app", Name: "Agent", RuntimeKind: agentcore.RuntimeNativeSDK}); err != nil {
		t.Fatal(err)
	}
	if err := memory.CreateRunWithMCP(context.Background(), &agentcore.AgentRun{ID: "run-auth", AppID: "app", AgentID: "agent", Status: agentcore.RunStatusRunning}, stored); err != nil {
		t.Fatal(err)
	}
	registry, allowed, authState, closeTools, err := PrepareRunTools(context.Background(), memory, tools.NewRegistry(), "app", "run-auth", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTools()
	authorized.Store(false)
	result, err := NewGatewayWithAllowed(memory, registry, allowed).CallTool(context.Background(), "app", "run-auth", ToolCallRequest{ToolName: "mcp__customer_io__read", Input: json.RawMessage(`{}`)})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("expected model-visible unauthorized tool error, result=%#v err=%v", result, err)
	}
	failure := authState.Failure()
	if failure == nil || failure.ServerID != "mcp-1" || failure.Reason != "unauthorized" {
		t.Fatalf("authentication failure = %#v", failure)
	}
}
