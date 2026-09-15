package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/helpin-ai/agent-runtime-go"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/appconfig"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestAPIStartRunAndReadMessages(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK", "true")
	mem := store.NewMemory()
	eng := engine.New(engine.Config{
		DefaultExecutionMode: engine.ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(runtime.NewNativeAdapter()),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
	})
	handler := NewServer(Config{Engine: eng, Store: mem, AllowAnonymous: true})

	agent := postJSON[agentcore.Agent](t, handler, "/internal/agents", map[string]interface{}{
		"app_id":                  "app-a",
		"name":                    "Agent",
		"runtime_kind":            "native_sdk",
		"allowed_tools":           []string{"get_context"},
		"allowed_targets":         []string{"ticket"},
		"approval_mode":           "never",
		"default_invocation_mode": "autonomous",
	}, http.StatusCreated)

	run := postJSON[agentcore.AgentRun](t, handler, "/internal/runs", map[string]interface{}{
		"app_id":   "app-a",
		"agent_id": agent.ID,
		"target": map[string]interface{}{
			"type": "ticket",
			"id":   "T-1",
		},
	}, http.StatusAccepted)

	waitForAPIStatus(t, mem, "app-a", run.ID, agentcore.RunStatusCompleted)
	req := httptest.NewRequest(http.MethodGet, "/internal/runs/"+run.ID+"/messages?app_id=app-a", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var messages []agentcore.AgentRunMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &messages); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
}

func TestAPIStartRunRejectsEventProtocolMismatch(t *testing.T) {
	handler := NewServer(Config{
		Engine:         engine.New(engine.Config{Store: store.NewMemory()}),
		Store:          store.NewMemory(),
		AllowAnonymous: true,
		AppConfig: &appconfig.Config{Apps: []appconfig.App{{
			AppID:         "helpin",
			EventProtocol: "v2",
		}}},
	})
	body := bytes.NewBufferString(`{"app_id":"helpin","agent_id":"agent-1","target":{"type":"repository","id":"repo-1"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(sdk.EventProtocolHeader, "v1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "host expects v1") {
		t.Fatalf("expected protocol mismatch, got status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIStartRunAcceptsRunMCPWithoutEchoingCredential(t *testing.T) {
	mem := store.NewMemory()
	agent := &agentcore.Agent{ID: "agent-mcp", AppID: "app-a", Name: "Agent", RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTargets: []string{"workspace"}}
	if err := mem.CreateAgent(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	eng := engine.New(engine.Config{
		Store: mem, Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider(),
		Runtimes: runtime.NewRegistry(runtime.NewNativeAdapter()), RunMCP: mcp.RunConfig{CredentialKey: key}, Durable: noopDurableExecutor{},
	})
	handler := NewServer(Config{Engine: eng, Store: mem, Tools: tools.NewRegistry(), AllowAnonymous: true})
	body := bytes.NewBufferString(`{
		"app_id":"app-a","agent_id":"agent-mcp","execution_mode":"durable","target":{"type":"workspace","id":"ws-1"},
		"mcp_servers":[{"server_id":"workspace-mcp-1","server_name":"github","transport":"streamable_http",
		"url":"https://mcp.example.com/mcp","tools":[{"name":"get_issue","access":"read"}],
		"skills":[{"key":"github_triage"}],
		"credential":{"type":"bearer_token","access_token":"run-secret"}}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "run-secret") || strings.Contains(rec.Body.String(), "credential") {
		t.Fatalf("start response leaked MCP credential: %s", rec.Body.String())
	}
	var run agentcore.AgentRun
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	servers, err := mem.ListRunMCPServers(context.Background(), "app-a", run.ID)
	if err != nil || len(servers) != 1 || len(servers[0].Skills) != 1 || servers[0].Skills[0].Key != "github_triage" || len(servers[0].EncryptedCredential) == 0 {
		t.Fatalf("stored MCP servers=%#v err=%v", servers, err)
	}
	if strings.Contains(string(servers[0].EncryptedCredential), "run-secret") {
		t.Fatal("stored MCP credential is plaintext")
	}
}

func TestAPIUpdatesOnlyExistingRunMCPCredential(t *testing.T) {
	mem := store.NewMemory()
	key := []byte("0123456789abcdef0123456789abcdef")
	run := &agentcore.AgentRun{
		ID: "run-mcp-rotate", AppID: "app-a", AgentID: "agent-a",
		Target: agentcore.TargetRef{Type: "workspace", ID: "ws-1"},
	}
	servers, err := mcp.PrepareStoredServers("app-a", run.ID, []mcp.RunServerRequest{{
		ServerID: "customer:io", ServerName: "customer_io",
		Transport: agentcore.MCPTransportStreamableHTTP, URL: "https://mcp.customer.io/mcp",
		Tools:      []mcp.RunTool{{Name: "cio_read_api", Access: agentcore.MCPToolAccessRead}},
		Credential: &mcp.RunCredential{Type: mcp.CredentialBearerToken, AccessToken: "old-secret"},
	}}, mcp.RunConfig{CredentialKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.CreateRunWithMCP(context.Background(), run, servers); err != nil {
		t.Fatal(err)
	}
	before, _ := mem.ListRunMCPServers(context.Background(), "app-a", run.ID)
	handler := NewServer(Config{
		Engine: engine.New(engine.Config{Store: mem, RunMCP: mcp.RunConfig{CredentialKey: key}}),
		Store:  mem, AllowAnonymous: true,
	})
	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	body := bytes.NewBufferString(`{"credential":{"type":"bearer_token","access_token":"rotated-secret","expires_at":"` + expiresAt + `"}}`)
	req := httptest.NewRequest(
		http.MethodPut,
		"/v1/runs/"+run.ID+"/mcp-servers/customer:io/credential?app_id=app-a",
		body,
	)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "rotated-secret") || strings.Contains(rec.Body.String(), "credential") {
		t.Fatalf("credential update response leaked a secret: %s", rec.Body.String())
	}
	after, _ := mem.ListRunMCPServers(context.Background(), "app-a", run.ID)
	if len(after) != 1 || string(after[0].EncryptedCredential) == string(before[0].EncryptedCredential) {
		t.Fatalf("credential was not replaced: before=%x after=%x", before[0].EncryptedCredential, after[0].EncryptedCredential)
	}

	body = bytes.NewBufferString(`{"credential":{"type":"bearer_token","access_token":"other-secret"}}`)
	req = httptest.NewRequest(
		http.MethodPut,
		"/v1/runs/"+run.ID+"/mcp-servers/not-present/credential?app_id=app-a",
		body,
	)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("expected exact server scoping, status=%d body=%s", rec.Code, rec.Body.String())
	}
}

type noopDurableExecutor struct{}

func (noopDurableExecutor) StartRun(context.Context, *agentcore.AgentRun) error  { return nil }
func (noopDurableExecutor) CancelRun(context.Context, *agentcore.AgentRun) error { return nil }
func (noopDurableExecutor) ResumeRun(context.Context, *agentcore.AgentRun, engine.ResumePayload) error {
	return nil
}

func TestAPIStartRunRejectsUnknownMCPFields(t *testing.T) {
	handler := NewServer(Config{Engine: engine.New(engine.Config{Store: store.NewMemory()}), Store: store.NewMemory(), AllowAnonymous: true})
	body := bytes.NewBufferString(`{"app_id":"app-a","agent_id":"agent","target":{"type":"workspace","id":"ws"},"mcp_servers":[{"server_id":"s","server_name":"n","transport":"streamable_http","url":"https://example.com/mcp","tools":[{"name":"read","access":"read","unexpected":true}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", body)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected strict schema rejection, got status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIListToolCalls(t *testing.T) {
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-tools",
		AppID:       "app-a",
		AgentID:     "agent-a",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	}
	if err := mem.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := mem.AppendToolCall(context.Background(), &agentcore.ToolCall{
		AppID:    "app-a",
		RunID:    "run-tools",
		ToolName: "run_command",
		Input:    json.RawMessage(`{"command":"go test ./..."}`),
		Output:   json.RawMessage(`{"summary":"ok"}`),
		Mutating: true,
	}); err != nil {
		t.Fatalf("append tool call: %v", err)
	}
	handler := NewServer(Config{
		Store:        mem,
		Engine:       engine.New(engine.Config{Store: mem}),
		Tools:        tools.NewRegistry(),
		ServiceToken: "secret",
	})

	req := httptest.NewRequest(http.MethodGet, "/internal/runs/run-tools/tool-calls?app_id=app-a", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized internal call, got %d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/internal/runs/run-tools/tool-calls?app_id=app-a", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected authorized internal ok, got %d body=%s", rec.Code, rec.Body.String())
	}
	var calls []agentcore.ToolCall
	if err := json.Unmarshal(rec.Body.Bytes(), &calls); err != nil {
		t.Fatalf("decode calls: %v", err)
	}
	if len(calls) != 1 || calls[0].ToolName != "run_command" || !calls[0].Mutating {
		t.Fatalf("unexpected calls: %#v", calls)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/runs/run-tools/tool-calls?app_id=app-a", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized v1 call, got %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/run-tools/tool-calls?app_id=app-a", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected authorized v1 ok, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIRunSearchEventHistoryAndExecutionDetail(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID: "run-observe", AppID: "app-a", AgentID: "agent-a",
		Target:      agentcore.TargetRef{Type: "task", ID: "task-1"},
		RuntimeKind: agentcore.RuntimeNativeSDK, ExecutionMode: engine.ExecutionModeLightweight,
		Status: agentcore.RunStatusCompleted,
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := mem.AppendEvent(ctx, &agentcore.AgentRunEvent{EventID: "event-1", AppID: "app-a", RunID: run.ID, Type: "run.completed"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	handler := NewServer(Config{Store: mem, Engine: engine.New(engine.Config{Store: mem}), Tools: tools.NewRegistry(), AllowAnonymous: true})

	searchReq := httptest.NewRequest(http.MethodGet, "/v1/runs/search?app_id=app-a&q=task-1&limit=10", nil)
	searchRec := httptest.NewRecorder()
	handler.ServeHTTP(searchRec, searchReq)
	if searchRec.Code != http.StatusOK {
		t.Fatalf("search status=%d body=%s", searchRec.Code, searchRec.Body.String())
	}
	var page agentcore.RunPage
	if err := json.Unmarshal(searchRec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != run.ID {
		t.Fatalf("unexpected page: %#v", page)
	}

	eventsReq := httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/events/history?app_id=app-a", nil)
	eventsRec := httptest.NewRecorder()
	handler.ServeHTTP(eventsRec, eventsReq)
	if eventsRec.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", eventsRec.Code, eventsRec.Body.String())
	}
	var events []agentcore.AgentRunEvent
	if err := json.Unmarshal(eventsRec.Body.Bytes(), &events); err != nil || len(events) != 1 || events[0].EventID != "event-1" {
		t.Fatalf("unexpected events: %#v err=%v", events, err)
	}

	executionReq := httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/execution?app_id=app-a", nil)
	executionRec := httptest.NewRecorder()
	handler.ServeHTTP(executionRec, executionReq)
	if executionRec.Code != http.StatusOK || !strings.Contains(executionRec.Body.String(), `"execution_mode":"lightweight"`) {
		t.Fatalf("execution status=%d body=%s", executionRec.Code, executionRec.Body.String())
	}
}

func TestAPIAppendAndListArtifacts(t *testing.T) {
	mem := store.NewMemory()
	handler := NewServer(Config{
		Store:        mem,
		Engine:       engine.New(engine.Config{Store: mem}),
		Tools:        tools.NewRegistry(),
		ServiceToken: "secret",
	})

	artifact := postJSON[agentcore.AgentRunArtifact](t, handler, "/v1/runs/run-artifacts/artifacts?app_id=app-a", map[string]interface{}{
		"artifact_type":  "usermaven_visual_report",
		"format":         "json",
		"storage_mode":   "inline",
		"inline_content": `{"report_type":"trend"}`,
		"metadata": map[string]interface{}{
			"source": "usermaven",
		},
	}, http.StatusCreated, withBearer("secret"))
	if artifact.AppID != "app-a" || artifact.RunID != "run-artifacts" || artifact.SequenceNo != 1 {
		t.Fatalf("unexpected artifact: %#v", artifact)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/runs/run-artifacts/artifacts?app_id=app-a", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", rec.Code, rec.Body.String())
	}
	var artifacts []agentcore.AgentRunArtifact
	if err := json.Unmarshal(rec.Body.Bytes(), &artifacts); err != nil {
		t.Fatalf("decode artifacts: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].ArtifactType != "usermaven_visual_report" {
		t.Fatalf("unexpected artifacts: %#v", artifacts)
	}
}

func TestV2RunEventsAreAppScopedAndSequenceReplayable(t *testing.T) {
	mem := store.NewMemory()
	run := &agentcore.AgentRun{ID: "run-v2", AppID: "helpin"}
	if err := mem.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, eventType := range []string{"assistant_message_started", "assistant_message_delta", "assistant_message_completed"} {
		if err := mem.AppendEvent(context.Background(), &agentcore.AgentRunEvent{
			AppID: "helpin", RunID: run.ID, Type: eventType,
			Data: map[string]interface{}{"message_id": "message-1", "content": "hello"},
		}); err != nil {
			t.Fatalf("append event: %v", err)
		}
	}
	handler := NewServer(Config{
		Store:  mem,
		Engine: engine.New(engine.Config{Store: mem}),
		Tools:  tools.NewRegistry(), AllowAnonymous: true,
		AppConfig: &appconfig.Config{Apps: []appconfig.App{{AppID: "helpin", EventProtocol: "v2"}}},
	})

	req := httptest.NewRequest(http.MethodGet, "/v2/runs/run-v2/events?app_id=helpin&after_sequence=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("v2 events status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response v2EventListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Events) != 2 || response.Events[0].SequenceNo != 2 || response.Events[0].SchemaVersion != engine.EventSchemaVersionV2 || response.NextSequenceNo != 3 {
		t.Fatalf("unexpected response: %#v", response)
	}

	pagedReq := httptest.NewRequest(http.MethodGet, "/v2/runs/run-v2/events?app_id=helpin&after_sequence=1&page_size=1", nil)
	pagedRec := httptest.NewRecorder()
	handler.ServeHTTP(pagedRec, pagedReq)
	if pagedRec.Code != http.StatusOK {
		t.Fatalf("paged v2 events status=%d body=%s", pagedRec.Code, pagedRec.Body.String())
	}
	var pagedResponse v2EventListResponse
	if err := json.Unmarshal(pagedRec.Body.Bytes(), &pagedResponse); err != nil {
		t.Fatalf("decode paged response: %v", err)
	}
	if len(pagedResponse.Events) != 1 || pagedResponse.Events[0].SequenceNo != 2 || pagedResponse.NextSequenceNo != 2 || pagedResponse.StreamStateSnapshot != nil {
		t.Fatalf("unexpected paged response: %#v", pagedResponse)
	}

	invalidPageReq := httptest.NewRequest(http.MethodGet, "/v2/runs/run-v2/events?app_id=helpin&page_size=0", nil)
	invalidPageRec := httptest.NewRecorder()
	handler.ServeHTTP(invalidPageRec, invalidPageReq)
	if invalidPageRec.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid page size to return 400, status=%d body=%s", invalidPageRec.Code, invalidPageRec.Body.String())
	}

	legacyReq := httptest.NewRequest(http.MethodGet, "/v2/runs/run-v2/events?app_id=usermaven", nil)
	legacyRec := httptest.NewRecorder()
	handler.ServeHTTP(legacyRec, legacyReq)
	if legacyRec.Code != http.StatusNotFound {
		t.Fatalf("expected v2 to remain disabled for usermaven, status=%d", legacyRec.Code)
	}
}

func TestV1RoutesRequireServiceTokenWhenConfigured(t *testing.T) {
	mem := store.NewMemory()
	handler := NewServer(Config{
		Store:        mem,
		Engine:       engine.New(engine.Config{Store: mem}),
		Tools:        tools.NewRegistry(),
		ServiceToken: "secret",
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/agents?app_id=app-a", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/agents?app_id=app-a", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProtectedRoutesFailClosedWhenServiceTokenMissing(t *testing.T) {
	mem := store.NewMemory()
	handler := NewServer(Config{
		Store:  mem,
		Engine: engine.New(engine.Config{Store: mem}),
		Tools:  tools.NewRegistry(),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/agents?app_id=app-a", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected service unavailable, got %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/internal/agents?app_id=app-a", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected service unavailable internal call, got %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected healthz to remain open, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadinessTracksProvidersWithoutChangingLiveness(t *testing.T) {
	registry := tools.NewRegistry()
	registry.SetProviderHealth(tools.ProviderHealth{AppID: "helpin", Provider: "helpin", Ready: false, Degraded: true, Source: "unavailable"})
	handler := NewServer(Config{Tools: registry})
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusServiceUnavailable} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s status=%d want=%d body=%s", path, rec.Code, want, rec.Body.String())
		}
	}
	registry.SetProviderHealth(tools.ProviderHealth{AppID: "helpin", Provider: "helpin", Ready: true, Source: "mcp"})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid provider catalog should satisfy readiness: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestInternalRoutesRequireServiceTokenWhenConfigured(t *testing.T) {
	mem := store.NewMemory()
	handler := NewServer(Config{
		Store:        mem,
		Engine:       engine.New(engine.Config{Store: mem}),
		Tools:        tools.NewRegistry(),
		ServiceToken: "secret",
	})

	for _, path := range []string{
		"/internal/capabilities",
		"/internal/agents?app_id=app-a",
		"/internal/agents/agent-a?app_id=app-a",
		"/internal/runs?app_id=app-a",
		"/internal/runs/run-a?app_id=app-a",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s expected unauthorized, got %d body=%s", path, rec.Code, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/internal/agents?app_id=app-a", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected authorized internal ok, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIAgentGetAndUpsert(t *testing.T) {
	mem := store.NewMemory()
	handler := NewServer(Config{
		Store:        mem,
		Engine:       engine.New(engine.Config{Store: mem}),
		Tools:        tools.NewRegistry(),
		ServiceToken: "secret",
	})

	created := putJSON[agentcore.Agent](t, handler, "/v1/agents/agent-a?app_id=app-a", map[string]interface{}{
		"name":                    "Native analytics",
		"runtime_kind":            "native_sdk",
		"provider":                "openai",
		"model":                   "gpt-4.1-mini",
		"allowed_tools":           []string{"analytics.query_trends"},
		"allowed_targets":         []string{"message_generation_task"},
		"approval_mode":           "never",
		"default_invocation_mode": "interactive",
	}, http.StatusCreated, withBearer("secret"))
	if created.ID != "agent-a" || created.AppID != "app-a" || created.Provider != "openai" {
		t.Fatalf("unexpected created agent: %#v", created)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/agents/agent-a?app_id=app-a", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected get ok, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got agentcore.Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.ID != "agent-a" || got.Model != "gpt-4.1-mini" {
		t.Fatalf("unexpected get: %#v", got)
	}

	updated := putJSON[agentcore.Agent](t, handler, "/v1/agents/agent-a?app_id=app-a", map[string]interface{}{
		"name":                    "Codex builder",
		"runtime_kind":            "native_sdk",
		"provider":                "openai",
		"model":                   "gpt-5-mini",
		"allowed_tools":           []string{"update_plan"},
		"allowed_targets":         []string{"repository"},
		"approval_mode":           "never",
		"default_invocation_mode": "interactive",
	}, http.StatusOK, withBearer("secret"))
	if updated.RuntimeKind != agentcore.RuntimeNativeSDK || updated.Model != "gpt-5-mini" {
		t.Fatalf("unexpected updated agent: %#v", updated)
	}
}

type requestOption func(*http.Request)

func withBearer(token string) requestOption {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func postJSON[T any](t *testing.T, handler http.Handler, path string, body interface{}, wantStatus int, opts ...requestOption) T {
	t.Helper()
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("POST %s status = %d body = %s", path, rec.Code, rec.Body.String())
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func putJSON[T any](t *testing.T, handler http.Handler, path string, body interface{}, wantStatus int, opts ...requestOption) T {
	t.Helper()
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("expected status %d, got %d body=%s", wantStatus, rec.Code, rec.Body.String())
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	return out
}

func waitForAPIStatus(t *testing.T, mem *store.Memory, appID, runID, status string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := mem.GetRun(context.Background(), appID, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run != nil && run.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run did not reach status %q", status)
}
