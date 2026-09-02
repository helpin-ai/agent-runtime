package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestGatewayListsAndCallsAllowedTools(t *testing.T) {
	ctx := context.Background()
	mem, registry, run := setupGatewayTest(t, agentcore.ApprovalModeNever)

	items, err := NewGateway(mem, registry).ListTools(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(items) != 1 || items[0].Name != "lookup_customer" {
		t.Fatalf("expected lookup_customer only, got %#v", items)
	}

	result, err := NewGateway(mem, registry).CallTool(ctx, "app-a", run.ID, ToolCallRequest{
		ToolName: "mcp__agent_runtime__lookup_customer",
		Input:    json.RawMessage(`{"id":"C-1"}`),
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected successful result: %#v", result)
	}
	if len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "Acme") {
		t.Fatalf("unexpected result content: %#v", result.Content)
	}
}

func TestGatewayDecodesTextToolResult(t *testing.T) {
	ctx := context.Background()
	mem, registry, run := setupGatewayTest(t, agentcore.ApprovalModeNever)
	registry.Register(tools.Definition{
		Name:        "read_text",
		Description: "Read text.",
		InputSchema: map[string]any{"type": "object"},
	}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		encoded, err := json.Marshal("line one\nline two")
		return encoded, err
	})
	agent, err := mem.GetAgent(ctx, "app-a", run.AgentID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	agent.AllowedTools = append(agent.AllowedTools, "read_text")
	if err := mem.UpdateAgent(ctx, agent); err != nil {
		t.Fatalf("update agent: %v", err)
	}

	result, err := NewGateway(mem, registry).CallTool(ctx, "app-a", run.ID, ToolCallRequest{
		ToolName: "read_text",
		Input:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("call text tool: %v", err)
	}
	if result.IsError || len(result.Content) != 1 || result.Content[0].Text != "line one\nline two" {
		t.Fatalf("expected decoded text content, got %#v", result)
	}
}

func TestGatewayRequiresApprovalForMutatingTools(t *testing.T) {
	ctx := context.Background()
	mem, registry, run := setupGatewayTest(t, agentcore.ApprovalModeMutatingTools)
	agent, err := mem.GetAgent(ctx, "app-a", run.AgentID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	agent.AllowedTools = append(agent.AllowedTools, "close_ticket")
	if err := mem.UpdateAgent(ctx, agent); err != nil {
		t.Fatalf("update agent: %v", err)
	}

	result, err := NewGateway(mem, registry).CallTool(ctx, "app-a", run.ID, ToolCallRequest{
		ToolName: "close_ticket",
		Input:    json.RawMessage(`{"resolution":"done"}`),
	})
	if err != nil {
		t.Fatalf("call mutating tool: %v", err)
	}
	if !result.ApprovalRequired || result.InteractionID == "" {
		t.Fatalf("expected approval-required response, got %#v", result)
	}
	interactions, err := mem.ListInteractions(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != "approval_request" {
		t.Fatalf("expected approval interaction, got %#v", interactions)
	}
	run.ApprovalState = agentcore.ApprovalApproved
	if err := mem.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	result, err = NewGateway(mem, registry).CallTool(ctx, "app-a", run.ID, ToolCallRequest{ToolName: "close_ticket", Input: json.RawMessage(`{"resolution":"done"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.ApprovalRequired || result.IsError || !strings.Contains(result.Content[0].Text, "closed") {
		t.Fatalf("approved mutating tool did not execute: %#v", result)
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ApprovalState != agentcore.ApprovalNotRequired {
		t.Fatalf("one-shot tool approval was not consumed: %#v", stored)
	}
	result, err = NewGateway(mem, registry).CallTool(ctx, "app-a", run.ID, ToolCallRequest{ToolName: "close_ticket", Input: json.RawMessage(`{"resolution":"again"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ApprovalRequired {
		t.Fatalf("a later mutating call must require a new approval: %#v", result)
	}
}

func TestGatewayRiskBasedModeExecutesRoutineAndGatesSensitiveMutations(t *testing.T) {
	ctx := context.Background()
	mem, registry, run := setupGatewayTest(t, agentcore.ApprovalModeRiskBased)
	agent, err := mem.GetAgent(ctx, "app-a", run.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	agent.AllowedTools = append(agent.AllowedTools, "save_note", "close_ticket")
	if err := mem.UpdateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	registry.Register(tools.Definition{
		Name: "save_note", InputSchema: map[string]interface{}{"type": "object"},
		Mutating: true, RiskLevel: tools.RiskLevelRoutine,
	}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"saved":true}`), nil
	})

	routine, err := NewGateway(mem, registry).CallTool(ctx, "app-a", run.ID, ToolCallRequest{ToolName: "save_note", Input: json.RawMessage(`{}`)})
	if err != nil || routine.ApprovalRequired || routine.IsError {
		t.Fatalf("routine mutation should execute directly: result=%#v err=%v", routine, err)
	}
	sensitive, err := NewGateway(mem, registry).CallTool(ctx, "app-a", run.ID, ToolCallRequest{ToolName: "close_ticket", Input: json.RawMessage(`{}`)})
	if err != nil || !sensitive.ApprovalRequired || sensitive.InteractionID == "" {
		t.Fatalf("sensitive mutation should require approval: result=%#v err=%v", sensitive, err)
	}
	run.ApprovalState = agentcore.ApprovalApproved
	if err := mem.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	approved, err := NewGateway(mem, registry).CallTool(ctx, "app-a", run.ID, ToolCallRequest{ToolName: "close_ticket", Input: json.RawMessage(`{}`)})
	if err != nil || approved.ApprovalRequired || approved.IsError {
		t.Fatalf("approved sensitive mutation should execute once: result=%#v err=%v", approved, err)
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ApprovalState != agentcore.ApprovalNotRequired {
		t.Fatalf("risk-based approval was not consumed: %#v", stored)
	}
}

func TestRegisterProviderToolsRegistersExternalMCPTools(t *testing.T) {
	ctx := context.Background()
	registry := tools.NewRegistry()
	provider := &fakeProvider{
		tools: []Tool{{
			Name:        "search",
			Description: "Search external context.",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"properties":{"query":{"type":"string"}}
			}`),
		}},
		result: &CallResult{Content: []ContentItem{{Type: "text", Text: "external result"}}},
	}

	names, err := RegisterProviderTools(ctx, registry, provider, "docs")
	if err != nil {
		t.Fatalf("register provider tools: %v", err)
	}
	if len(names) != 1 || names[0] != "docs__search" {
		t.Fatalf("unexpected registered names: %#v", names)
	}
	out, err := registry.Execute(ctx, tools.CallContext{}, "docs__search", json.RawMessage(`{"query":"refund"}`))
	if err != nil {
		t.Fatalf("execute provider tool: %v", err)
	}
	if !strings.Contains(string(out), "external result") {
		t.Fatalf("unexpected provider output: %s", string(out))
	}
	if provider.calledName != "search" {
		t.Fatalf("expected original provider tool name, got %q", provider.calledName)
	}
	if provider.calledMeta.AppID != "" || provider.calledMeta.RunID != "" {
		t.Fatalf("expected empty metadata for direct registry call without context, got %#v", provider.calledMeta)
	}
	run := &agentcore.AgentRun{
		ID:              "run-1",
		AppID:           "app-a",
		AgentID:         "agent-1",
		ExternalActorID: "user-1",
		Target: agentcore.TargetRef{
			Type:     "workspace",
			ID:       "ws-target",
			Metadata: map[string]interface{}{"workspace_id": "ws-1"},
		},
		Input: agentcore.RunInput{Metadata: map[string]interface{}{"workspace_id": "ws-input"}},
	}
	if _, err := registry.Execute(ctx, tools.CallContext{AppID: "app-a", RunID: "run-1", Run: run}, "docs__search", json.RawMessage(`{"query":"refund"}`)); err != nil {
		t.Fatalf("execute provider tool with context: %v", err)
	}
	if provider.calledMeta.AppID != "app-a" ||
		provider.calledMeta.RunID != "run-1" ||
		provider.calledMeta.AgentID != "agent-1" ||
		provider.calledMeta.ExternalActorID != "user-1" ||
		provider.calledMeta.WorkspaceID != "ws-input" ||
		provider.calledMeta.TargetType != "workspace" ||
		provider.calledMeta.TargetID != "ws-target" {
		t.Fatalf("unexpected provider metadata: %#v", provider.calledMeta)
	}
}

func TestProviderRegistrationPreservesStructuredContentAndIgnoresTargetMetadata(t *testing.T) {
	ctx := context.Background()
	registry := tools.NewRegistry()
	provider := &fakeProvider{
		tools: []Tool{{
			Name:                 "ai.get_task_status",
			InputSchema:          json.RawMessage(`{"type":"object"}`),
			Mutating:             true,
			SupportedTargetTypes: []string{"message_generation_task"},
		}},
		result: &CallResult{StructuredContent: json.RawMessage(`{"status":"complete"}`)},
	}
	registrations, _, err := ProviderRegistrations(ctx, provider, "usermaven")
	if err != nil {
		t.Fatal(err)
	}
	if got := registrations[0].Definition.SupportedTargetTypes; len(got) != 0 {
		t.Fatalf("provider target metadata must remain unenforced in this release: %#v", got)
	}
	if err := registry.ReplaceAppProvider("usermaven", "usermaven", 0, registrations, false); err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemory()
	agent := &agentcore.Agent{AppID: "usermaven", Name: "Maven", AllowedTools: []string{"usermaven__ai.get_task_status"}}
	if err := mem.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{AppID: "usermaven", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "workspace", ID: "workspace-1"}, Status: agentcore.RunStatusRunning}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	listed, err := NewGateway(mem, registry).ListTools(ctx, "usermaven", run.ID)
	if err != nil || len(listed) != 1 || listed[0].Name != "usermaven__ai.get_task_status" {
		t.Fatalf("Usermaven tool must remain visible on workspace targets: tools=%#v err=%v", listed, err)
	}
	if got := registrations[0].Definition.EffectiveRiskLevel(); got != tools.RiskLevelSensitive {
		t.Fatalf("missing provider risk must retain sensitive mutation fallback, got %q", got)
	}
	out, err := registry.Execute(ctx, tools.CallContext{AppID: "usermaven"}, "usermaven__ai.get_task_status", nil)
	if err != nil || string(out) != `{"status":"complete"}` {
		t.Fatalf("structured result was not preserved: output=%s err=%v", out, err)
	}
}

func setupGatewayTest(t *testing.T, approvalMode string) (*store.Memory, *tools.Registry, *agentcore.AgentRun) {
	t.Helper()
	ctx := context.Background()
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	registry.Register(tools.Definition{
		Name:        "lookup_customer",
		Description: "Lookup customer context.",
		Category:    "Context",
		InputSchema: map[string]interface{}{"type": "object"},
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"name":"Acme"}`), nil
	})
	registry.Register(tools.Definition{
		Name:        "close_ticket",
		Description: "Close a ticket.",
		Category:    "Ticket",
		InputSchema: map[string]interface{}{"type": "object"},
		Mutating:    true,
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"closed":true}`), nil
	})

	agent := &agentcore.Agent{
		AppID:                 "app-a",
		Name:                  "Gateway Agent",
		RuntimeKind:           agentcore.RuntimeNativeSDK,
		AllowedTools:          []string{"lookup_customer"},
		AllowedTargets:        []string{"ticket"},
		ApprovalMode:          approvalMode,
		DefaultInvocationMode: agentcore.InvocationAutonomous,
	}
	if err := mem.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:          "app-a",
		AgentID:        agent.ID,
		Target:         agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:    agentcore.RuntimeNativeSDK,
		ExecutionMode:  "lightweight",
		InvocationMode: agentcore.InvocationAutonomous,
		Status:         agentcore.RunStatusRunning,
		PauseReason:    agentcore.PauseReasonNone,
		ApprovalState:  agentcore.ApprovalNotRequired,
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return mem, registry, run
}

type fakeProvider struct {
	tools      []Tool
	result     *CallResult
	calledName string
	calledMeta tools.CommandExecutionContext
}

func (p *fakeProvider) ListTools(context.Context) ([]Tool, error) {
	return p.tools, nil
}

func (p *fakeProvider) CallTool(_ context.Context, name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error) {
	p.calledName = name
	p.calledMeta = meta
	return p.result, nil
}
