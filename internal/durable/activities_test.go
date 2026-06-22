package durable

import (
	"context"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestAgentRunActivitiesExecuteRunUsesEnginePath(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := &agentcore.Agent{
		AppID:                 "app-a",
		Name:                  "Durable Agent",
		RuntimeKind:           agentcore.RuntimeNativeSDK,
		AllowedTargets:        []string{"ticket"},
		ApprovalMode:          agentcore.ApprovalModeNever,
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
		ExecutionMode:  engine.ExecutionModeDurable,
		InvocationMode: agentcore.InvocationAutonomous,
		Status:         agentcore.RunStatusQueued,
		PauseReason:    agentcore.PauseReasonNone,
		ApprovalState:  agentcore.ApprovalNotRequired,
		Input:          agentcore.RunInput{Instructions: "summarize"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	targets := host.NewStaticContextProvider()
	targets.Register("app-a", run.Target, host.TargetContext{Summary: "customer context"})
	runner := engine.New(engine.Config{
		DefaultExecutionMode: engine.ExecutionModeDurable,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(runtime.NewNativeAdapter()),
		Tools:                tools.NewRegistry(),
		Targets:              targets,
	})
	activities := NewAgentRunActivities(mem, runner)

	if err := activities.PrepareRunActivity(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("prepare run: %v", err)
	}
	result, err := activities.ExecuteRunActivity(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if result.WaitForApproval || result.AwaitingInput || result.AwaitingAuth {
		t.Fatalf("unexpected wait result: %#v", result)
	}
	updated, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status != agentcore.RunStatusCompleted {
		t.Fatalf("expected completed run, got %q", updated.Status)
	}
	messages, err := mem.ListMessages(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Role != "assistant" {
		t.Fatalf("expected assistant message, got %#v", messages)
	}
}
