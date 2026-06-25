package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestAPIStartRunAndReadMessages(t *testing.T) {
	mem := store.NewMemory()
	eng := engine.New(engine.Config{
		DefaultExecutionMode: engine.ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(runtime.NewNativeAdapter(), runtime.NewCodexAdapter()),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
	})
	handler := NewServer(Config{Engine: eng, Store: mem})

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
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", rec.Code, rec.Body.String())
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

func TestAPIStartCodexDeviceCodeAuth(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"requiresOpenaiAuth":true}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"type":"chatgptDeviceCode","loginId":"login-1","verificationUrl":"https://example.test/device","userCode":"WXYZ"}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	agent := &agentcore.Agent{ID: "agent-codex", AppID: "app-a", Name: "Codex", RuntimeKind: agentcore.RuntimeCodex, Provider: "openai", Model: "gpt"}
	if err := mem.CreateAgent(context.Background(), agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		ID:          "run-codex",
		AppID:       "app-a",
		AgentID:     agent.ID,
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	}
	if err := mem.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	authManager := runtime.NewCodexAuthManager(mem, runtime.CodexConfig{
		CommandPath:    command,
		Timeout:        time.Second,
		OpenAIAuthMode: "chatgpt_device_code",
		RuntimeRoot:    filepath.Join(tmp, "runtime"),
	})
	handler := NewServer(Config{
		Store:     mem,
		Engine:    engine.New(engine.Config{Store: mem}),
		Tools:     tools.NewRegistry(),
		CodexAuth: authManager,
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/runs/run-codex/codex-auth/device-code/start?app_id=app-a", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", rec.Code, rec.Body.String())
	}
	var state runtime.CodexAuthState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.State != "pending" || state.UserCode == nil || *state.UserCode != "WXYZ" {
		t.Fatalf("unexpected state: %#v", state)
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
