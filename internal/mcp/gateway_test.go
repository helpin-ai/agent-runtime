package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
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
		ExecutionMode:  engine.ExecutionModeLightweight,
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

func (p *fakeProvider) ListTools() ([]Tool, error) {
	return p.tools, nil
}

func (p *fakeProvider) CallTool(name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error) {
	p.calledName = name
	p.calledMeta = meta
	return p.result, nil
}
