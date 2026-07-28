package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestCodexDynamicToolSpecsUseEffectiveRunAllowlist(t *testing.T) {
	execCtx, _ := newCodexDynamicToolTestContext(t, []string{"web_search_exa", "fetch_url"}, []string{"fetch_url"})

	specs, err := codexDynamicToolSpecs(context.Background(), execCtx)
	if err != nil {
		t.Fatalf("build dynamic tool specs: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "fetch_url" {
		t.Fatalf("expected only fetch_url, got %#v", specs)
	}
	if specs[0].Type != "function" || !strings.Contains(string(specs[0].InputSchema), `"url"`) {
		t.Fatalf("unexpected dynamic tool spec: %#v", specs[0])
	}
}

func TestCodexWebSearchEnabledUsesEffectiveSearchPolicy(t *testing.T) {
	execCtx, _ := newCodexDynamicToolTestContext(t, []string{"web_search_exa", "fetch_url"}, []string{"web_search_exa"})
	if !codexWebSearchEnabled(execCtx) {
		t.Fatal("expected effective Exa permission to enable Codex web search")
	}
	if !strings.Contains((&CodexAdapter{}).codexDeveloperInstructions(execCtx), "built-in web search") {
		t.Fatal("expected Codex search compatibility guidance")
	}

	execCtx, _ = newCodexDynamicToolTestContext(t, []string{"web_search_exa", "fetch_url"}, []string{"fetch_url"})
	if codexWebSearchEnabled(execCtx) {
		t.Fatal("run-level tool narrowing must disable Codex web search")
	}
}

func TestCodexDynamicToolCallExecutesThroughGuardedGateway(t *testing.T) {
	execCtx, called := newCodexDynamicToolTestContext(t, []string{"fetch_url"}, nil)
	client := &fakeCodexRPC{}
	msg := codexRPCMessage{
		ID:     json.RawMessage(`41`),
		Method: "item/tool/call",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"fetch_url","arguments":{"url":"https://example.com/updates"}}`),
	}

	if err := (&CodexAdapter{}).handleCodexDynamicToolCall(context.Background(), client, execCtx, msg); err != nil {
		t.Fatalf("handle dynamic tool call: %v", err)
	}
	if got := *called; !strings.Contains(got, "https://example.com/updates") {
		t.Fatalf("tool input was not executed: %q", got)
	}
	if len(client.responds) != 1 || !strings.Contains(client.responds[0], `"success":true`) || !strings.Contains(client.responds[0], `"type":"inputText"`) {
		t.Fatalf("unexpected Codex response: %#v", client.responds)
	}

	toolCalls, err := execCtx.Store.ListToolCalls(context.Background(), execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(toolCalls) != 1 || toolCalls[0].ToolName != "fetch_url" {
		t.Fatalf("expected guarded gateway audit record, got %#v", toolCalls)
	}
}

func TestCodexAppServerAdvertisesAndRunsDynamicTools(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
case "$line" in
  *'"config"'*'"web_search":"live"'*'"dynamicTools"'*'"fetch_url"'*) ;;
  *) printf '%s\n' 'thread/start did not enable web search and contain fetch_url dynamic tool' >&2; exit 2 ;;
esac
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' '{"id":4,"method":"item/tool/call","params":{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"fetch_url","arguments":{"url":"https://example.com/updates"}}}'
IFS= read -r line
case "$line" in
  *'"id":4'*'"success":true'*'verified update'*) ;;
  *) printf '%s\n' 'dynamic tool response was invalid' >&2; exit 3 ;;
esac
printf '%s\n' '{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"msg-1","text":"digest created"}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	execCtx, called := newCodexDynamicToolTestContext(t, []string{"web_search_exa", "fetch_url"}, nil)
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     3 * time.Second,
		AppServer:   true,
	})

	result, err := adapter.Execute(execCtx)
	if err != nil {
		t.Fatalf("execute Codex app-server: %v", err)
	}
	if result.AssistantMessage != "digest created" {
		t.Fatalf("unexpected assistant result: %q", result.AssistantMessage)
	}
	if !strings.Contains(*called, "https://example.com/updates") {
		t.Fatalf("dynamic tool was not executed: %q", *called)
	}
}

func newCodexDynamicToolTestContext(t *testing.T, agentTools, runTools []string) (*ExecutionContext, *string) {
	t.Helper()
	ctx := context.Background()
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	called := ""
	registry.Register(tools.Definition{
		Name:        "fetch_url",
		Description: "Fetch and verify an exact URL.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{"type": "string"},
			},
			"required": []string{"url"},
		},
	}, func(_ context.Context, _ tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		called = string(input)
		return json.RawMessage(`{"status":"verified update"}`), nil
	})
	registry.Register(tools.Definition{
		Name:        "web_search_exa",
		Description: "Search the web.",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ tools.CallContext, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})
	agent := &agentcore.Agent{
		AppID:                 "app-a",
		Name:                  "Competitive digest",
		RuntimeKind:           agentcore.RuntimeCodex,
		AllowedTools:          append([]string(nil), agentTools...),
		ApprovalMode:          agentcore.ApprovalModeNever,
		DefaultInvocationMode: agentcore.InvocationAutonomous,
	}
	if err := mem.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:          "app-a",
		AgentID:        agent.ID,
		Target:         agentcore.TargetRef{Type: "workspace", ID: "workspace-1"},
		Input:          agentcore.RunInput{Instructions: "compile digest", AllowedTools: append([]string(nil), runTools...)},
		RuntimeKind:    agentcore.RuntimeCodex,
		ExecutionMode:  "lightweight",
		InvocationMode: agentcore.InvocationAutonomous,
		Status:         agentcore.RunStatusRunning,
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	allowed := tools.AllowedSet(agent, runTools)
	return &ExecutionContext{
		Context:      ctx,
		AppID:        "app-a",
		Agent:        agent,
		Run:          run,
		Store:        mem,
		AllowedTools: allowed,
		Tools:        registry,
	}, &called
}
