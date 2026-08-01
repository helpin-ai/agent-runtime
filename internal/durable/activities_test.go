package durable

import (
	"context"
	"errors"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"go.temporal.io/sdk/temporal"
)

func TestAgentRunActivitiesExecuteRunUsesEnginePath(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK", "true")
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

func TestAgentRunActivitiesPreservesSynthesizedApprovalPause(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := &agentcore.Agent{
		AppID:                 "app-a",
		Name:                  "Custom Approval Agent",
		RuntimeKind:           agentcore.RuntimeCodex,
		AllowedTools:          []string{"request_approval", "request_user_input"},
		AllowedTargets:        []string{"task"},
		Skills:                []agentcore.SkillRef{{Key: "approval_protocol"}},
		ApprovalMode:          agentcore.ApprovalModeNever,
		DefaultInvocationMode: agentcore.InvocationInteractive,
	}
	if err := mem.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		ID:             "run-durable-approval",
		AppID:          agent.AppID,
		AgentID:        agent.ID,
		Target:         agentcore.TargetRef{Type: "task", ID: "task-1"},
		RuntimeKind:    agentcore.RuntimeCodex,
		ExecutionMode:  engine.ExecutionModeDurable,
		InvocationMode: agentcore.InvocationInteractive,
		Status:         agentcore.RunStatusRunning,
		PauseReason:    agentcore.PauseReasonNone,
		ApprovalState:  agentcore.ApprovalNotRequired,
		Input:          agentcore.RunInput{Instructions: "publish a reviewable plan"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	runner := engine.New(engine.Config{
		DefaultExecutionMode: engine.ExecutionModeDurable,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(completingCodexAdapter{}),
		Tools:                tools.NewRegistry(),
		Skills:               skills.NewDefaultRegistry(),
		Targets:              host.NewStaticContextProvider(),
	})
	activities := NewAgentRunActivities(mem, runner)

	result, err := activities.ExecuteRunActivity(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if !result.WaitForApproval {
		t.Fatalf("expected durable activity to preserve synthesized approval, got %#v", result)
	}
	updated, err := mem.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status != agentcore.RunStatusPaused || updated.PauseReason != agentcore.PauseReasonHumanApproval {
		t.Fatalf("expected durable approval pause, got status=%q reason=%q", updated.Status, updated.PauseReason)
	}
	interactions, err := mem.ListInteractions(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != skills.InteractionKindApprovalRequest {
		t.Fatalf("expected pending durable approval interaction, got %#v", interactions)
	}
}

type completingCodexAdapter struct{}

func (completingCodexAdapter) Kind() string {
	return agentcore.RuntimeCodex
}

func (completingCodexAdapter) Execute(_ *runtime.ExecutionContext) (*runtime.Result, error) {
	return &runtime.Result{AssistantMessage: "The plan is published for review."}, nil
}

func TestDurableExecutionErrorRetriesOnlyContextInterruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	interrupted := durableExecutionError(ctx, context.Canceled)
	var interruptedApplication *temporal.ApplicationError
	if !errors.As(interrupted, &interruptedApplication) {
		t.Fatalf("expected application error, got %T: %v", interrupted, interrupted)
	}
	if interruptedApplication.Type() != workerInterruptedErrorType || interruptedApplication.NonRetryable() {
		t.Fatalf("unexpected interruption classification: %v", interruptedApplication)
	}

	failed := durableExecutionError(context.Background(), errors.New("provider rejected request"))
	var failedApplication *temporal.ApplicationError
	if !errors.As(failed, &failedApplication) {
		t.Fatalf("expected application error, got %T: %v", failed, failed)
	}
	if failedApplication.Type() != "RunExecutionFailed" || !failedApplication.NonRetryable() {
		t.Fatalf("unexpected failure classification: %v", failedApplication)
	}
}
