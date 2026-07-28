package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestStartRunCompletesLightweightNativeRun(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK", "true")
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	targets := host.NewStaticContextProvider()
	targets.Register("app-a", agentcore.TargetRef{Type: "ticket", ID: "T-1"}, host.TargetContext{Summary: "ticket context"})
	eng := testEngine(mem, targets)

	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Instructions: "summarize",
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusCompleted)
	if run.OutputSummary == nil {
		t.Fatal("expected output summary")
	}
	messages, err := mem.ListMessages(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Role != "assistant" {
		t.Fatalf("expected assistant message, got %#v", messages)
	}
}

func TestRunCompletionRequiresConfiguredToolCall(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		toolCalls []agentcore.ToolCall
		want      string
	}{
		{name: "missing required tool fails", want: agentcore.RunStatusFailed},
		{
			name:      "successful required tool completes",
			toolCalls: []agentcore.ToolCall{{ToolName: "publish_task_plan_doc"}},
			want:      agentcore.RunStatusCompleted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := store.NewMemory()
			agent := testAgent("app-a")
			agent.ExecutionConfig = json.RawMessage(`{"completion":{"required_tools":["publish_task_plan_doc"]}}`)
			if err := mem.CreateAgent(ctx, &agent); err != nil {
				t.Fatalf("create agent: %v", err)
			}
			targets := host.NewStaticContextProvider()
			targets.Register("app-a", agentcore.TargetRef{Type: "ticket", ID: "T-1"}, host.TargetContext{Summary: "task context"})
			adapter := &recordingRuntimeAdapter{toolCalls: tt.toolCalls}
			eng := New(Config{
				DefaultExecutionMode: ExecutionModeLightweight,
				Store:                mem,
				Runtimes:             runtime.NewRegistry(adapter),
				Tools:                tools.NewRegistry(),
				Targets:              targets,
			})
			run, err := eng.StartRun(ctx, StartRunRequest{
				AppID: "app-a", AgentID: agent.ID,
				Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"},
			})
			if err != nil {
				t.Fatalf("start run: %v", err)
			}
			stored := waitForRunStatus(t, mem, "app-a", run.ID, tt.want)
			if tt.want == agentcore.RunStatusFailed && !strings.Contains(stored.ErrorMessage, "publish_task_plan_doc") {
				t.Fatalf("expected missing completion tool error, got %q", stored.ErrorMessage)
			}
		})
	}
}

func TestStartRunPropagatesHostRunIDAndUsageEvents(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	events := &recordingEngineEventSink{}
	adapter := &recordingRuntimeAdapter{
		outputSummary: json.RawMessage(`{"input_tokens":3,"cached_input_tokens":1,"output_tokens":5}`),
	}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		EventSink:            events,
	})

	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		HostRunID:    "helpin-run-123",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Instructions: "summarize",
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusCompleted)
	if run.HostRunID != "helpin-run-123" {
		t.Fatalf("expected host run id to persist, got %q", run.HostRunID)
	}
	checkpoint := waitForEventType(t, events, "usage.checkpoint")
	if checkpoint.HostRunID != "helpin-run-123" || checkpoint.Data["host_run_id"] != "helpin-run-123" {
		t.Fatalf("usage checkpoint did not include host run id: %#v", checkpoint)
	}
	if got := usageTotalFromEvent(t, checkpoint); got != 9 {
		t.Fatalf("expected checkpoint total usage 9, got %d in %#v", got, checkpoint.Data)
	}
	completed := waitForEventType(t, events, "run.completed")
	if completed.HostRunID != "helpin-run-123" || completed.Data["host_run_id"] != "helpin-run-123" {
		t.Fatalf("completed event did not include host run id: %#v", completed)
	}
	if got := usageTotalFromEvent(t, completed); got != 9 {
		t.Fatalf("expected completed total usage 9, got %d in %#v", got, completed.Data)
	}

	retry, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		HostRunID:    "helpin-run-123",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-2"},
		Instructions: "retry after timeout",
	})
	if err != nil {
		t.Fatalf("retry start run: %v", err)
	}
	if retry.ID != run.ID {
		t.Fatalf("expected idempotent retry to return run %q, got %q", run.ID, retry.ID)
	}
	if adapter.calls != 1 {
		t.Fatalf("expected idempotent retry not to execute adapter again, calls=%d", adapter.calls)
	}
}

func TestStartRunWithPauseAfterAssistantPolicyPausesAfterAssistantTurn(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK", "true")
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	targets := host.NewStaticContextProvider()
	targets.Register("app-a", agentcore.TargetRef{Type: "ticket", ID: "T-1"}, host.TargetContext{Summary: "ticket context"})
	eng := testEngine(mem, targets)

	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Instructions: "answer",
		TurnPolicy: agentcore.TurnPolicy{
			Mode:               agentcore.TurnPolicyPauseAfterAssist,
			IdleTimeoutSeconds: 604800,
		},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusPaused)
	if run.PauseReason != agentcore.PauseReasonUserMessage {
		t.Fatalf("pause_reason = %q", run.PauseReason)
	}
	if run.CompletedAt != nil {
		t.Fatalf("expected non-terminal chat run, completed_at=%v", run.CompletedAt)
	}
	messages, err := mem.ListMessages(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Role != "assistant" || !strings.Contains(messages[0].Content, "completed a native_sdk run") {
		t.Fatalf("expected assistant message, got %#v", messages)
	}
}

func TestCumulativeOutputSummaryAddsNativeResumeUsage(t *testing.T) {
	base := json.RawMessage(`{"input_tokens":5,"cached_input_tokens":1,"output_tokens":7}`)
	current := json.RawMessage(`{"input_tokens":2,"output_tokens":3,"native_messages":[]}`)

	summary := cumulativeOutputSummary(base, current, agentcore.RuntimeNativeSDK)
	usage := usageFromSummary(summary)
	if usage.InputTokens != 7 || usage.CachedInputTokens != 1 || usage.OutputTokens != 10 || usage.TotalTokens != 18 {
		t.Fatalf("expected cumulative native usage, got summary=%s usage=%#v", string(summary), usage)
	}

	codex := cumulativeOutputSummary(base, current, agentcore.RuntimeCodex)
	if string(codex) != string(current) {
		t.Fatalf("expected codex summary to remain adapter cumulative value, got %s", string(codex))
	}
}

func TestResumeRunExpiresStalePausedChatRun(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	eng := testEngine(mem, host.NewStaticContextProvider())
	run := &agentcore.AgentRun{
		ID:          "run-stale-chat",
		AppID:       "app-a",
		AgentID:     agent.ID,
		Target:      agentcore.TargetRef{Type: "conversation", ID: "C-1"},
		RuntimeKind: agentcore.RuntimeNativeSDK,
		Status:      agentcore.RunStatusPaused,
		PauseReason: agentcore.PauseReasonUserMessage,
		Input: agentcore.RunInput{
			TurnPolicy: agentcore.TurnPolicy{
				Mode:               agentcore.TurnPolicyPauseAfterAssist,
				IdleTimeoutSeconds: 1,
			},
		},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)

	if _, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{Intent: "reply", Content: "continue"}); err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("expected idle timeout error, got %v", err)
	}
	run, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != agentcore.RunStatusCompleted || run.PauseReason != agentcore.PauseReasonNone || run.CompletedAt == nil {
		t.Fatalf("expected completed stale run, got %#v", run)
	}
}

func TestStartRunEnforcesAppIsolation(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	eng := testEngine(mem, host.NewStaticContextProvider())

	_, err := eng.StartRun(ctx, StartRunRequest{
		AppID:   "app-b",
		AgentID: agent.ID,
		Target:  agentcore.TargetRef{Type: "ticket", ID: "T-1"},
	})
	if err == nil {
		t.Fatal("expected app-scoped agent lookup to fail")
	}
}

func TestStartRunRejectsDisallowedToolSubset(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.AllowedTools = []string{"get_context"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	eng := testEngine(mem, host.NewStaticContextProvider())

	_, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		AllowedTools: []string{"delete_customer"},
	})
	if err == nil {
		t.Fatal("expected disallowed tool error")
	}
}

func TestApprovalModePausesAndApproveResumes(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK", "true")
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.ApprovalMode = agentcore.ApprovalModeAlways
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	eng := testEngine(mem, host.NewStaticContextProvider())

	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID:   "app-a",
		AgentID: agent.ID,
		Target:  agentcore.TargetRef{Type: "ticket", ID: "T-1"},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusPaused)
	if run.PauseReason != agentcore.PauseReasonHumanApproval {
		t.Fatalf("pause_reason = %q", run.PauseReason)
	}

	if _, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{Intent: "approve"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusCompleted)
	if run.ApprovalState != agentcore.ApprovalApproved {
		t.Fatalf("approval_state = %q", run.ApprovalState)
	}
}

func TestResumeRunResolvesPendingInteractionAndKeepsPausedOutputSummary(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	adapter := &pausingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
	})

	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Instructions: "ask",
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusPaused)
	if !strings.Contains(string(run.OutputSummary), `"native_messages"`) {
		t.Fatalf("expected paused output summary to be persisted, got %s", string(run.OutputSummary))
	}
	interactions, err := mem.ListInteractions(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].Status != "pending" {
		t.Fatalf("expected pending interaction, got %#v", interactions)
	}

	if _, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{
		Intent:          "reply",
		Content:         "Sales owns it.",
		ResponsePayload: json.RawMessage(`{"answers":{"owner":{"answers":["Sales"]}}}`),
		ExternalActorID: "user-1",
	}); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusCompleted)
	if adapter.calls != 2 {
		t.Fatalf("expected adapter to execute twice, got %d", adapter.calls)
	}
	interactions, err = mem.ListInteractions(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list interactions after resume: %v", err)
	}
	if len(interactions) != 1 || interactions[0].Status != "resolved" || interactions[0].ResolvedByExternalID != "user-1" {
		t.Fatalf("expected resolved interaction, got %#v", interactions)
	}
	if string(interactions[0].ResponsePayload) != `{"answers":{"owner":{"answers":["Sales"]}}}` {
		t.Fatalf("unexpected response payload: %s", string(interactions[0].ResponsePayload))
	}
	if run.Input.Metadata == nil || run.Input.Metadata["last_resume"] == nil {
		t.Fatalf("expected last_resume metadata, got %#v", run.Input.Metadata)
	}
}

func TestResumeRunPersistsResumePayloadBeforeDurableSignal(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		ID:            "run-durable-resume",
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeDurable,
		Status:        agentcore.RunStatusPaused,
		PauseReason:   agentcore.PauseReasonUserMessage,
		Input:         agentcore.RunInput{Metadata: map[string]interface{}{}},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	durable := &recordingDurableExecutor{store: mem}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeDurable,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(&recordingRuntimeAdapter{}),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Durable:              durable,
	})

	if _, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{Intent: "reply", Content: "continue"}); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if durable.resumeCalls != 1 {
		t.Fatalf("expected durable resume call, got %d", durable.resumeCalls)
	}
	if durable.runAtResume == nil || durable.runAtResume.Input.Metadata["last_resume"] == nil {
		t.Fatalf("expected durable signal after last_resume persisted, got %#v", durable.runAtResume)
	}
}

func TestResumeRunDeduplicatesLegacyRetryWithoutResumeID(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		ID:            "run-durable-deduplicate",
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeDurable,
		Status:        agentcore.RunStatusPaused,
		PauseReason:   agentcore.PauseReasonUserMessage,
		Input:         agentcore.RunInput{Metadata: map[string]interface{}{}},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	durable := &recordingDurableExecutor{store: mem}
	eng := New(Config{Store: mem, Durable: durable})
	payload := ResumePayload{Intent: "reply", Content: "continue"}
	resumed, err := eng.ResumeRun(ctx, "app-a", run.ID, payload)
	if err != nil {
		t.Fatalf("first resume: %v", err)
	}
	lastResume, _ := resumed.Input.Metadata["last_resume"].(map[string]interface{})
	generatedResumeID, _ := lastResume["resume_id"].(string)
	if !strings.HasPrefix(generatedResumeID, "auto:"+run.ID+":") {
		t.Fatalf("expected deterministic fallback resume id, got %q", generatedResumeID)
	}
	if _, err := eng.ResumeRun(ctx, "app-a", run.ID, payload); err != nil {
		t.Fatalf("duplicate resume: %v", err)
	}
	if durable.resumeCalls != 1 {
		t.Fatalf("expected one Temporal signal, got %d", durable.resumeCalls)
	}
	messages, err := mem.ListMessages(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].RuntimeMessageID != generatedResumeID {
		t.Fatalf("expected one resume-correlated message, got %#v", messages)
	}
}

func TestResumeRunRollsBackStateWhenDurableSignalFails(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		ID:            "run-durable-signal-failure",
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeDurable,
		Status:        agentcore.RunStatusPaused,
		PauseReason:   agentcore.PauseReasonHumanApproval,
		Input:         agentcore.RunInput{Metadata: map[string]interface{}{}},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	interaction := &agentcore.AgentRunInteraction{
		ID:              "interaction-1",
		AppID:           "app-a",
		RunID:           run.ID,
		InteractionKind: "approval_request",
		Status:          "pending",
	}
	if err := mem.AppendInteraction(ctx, interaction); err != nil {
		t.Fatalf("append interaction: %v", err)
	}
	durable := &recordingDurableExecutor{store: mem, resumeErr: errors.New("temporal unavailable")}
	eng := New(Config{Store: mem, Durable: durable})
	_, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{
		Intent:        "approve",
		ResumeID:      "resume-1",
		InteractionID: interaction.ID,
	})
	if err == nil || !strings.Contains(err.Error(), "temporal unavailable") {
		t.Fatalf("expected Temporal signal error, got %v", err)
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.Status != agentcore.RunStatusPaused || stored.PauseReason != agentcore.PauseReasonHumanApproval {
		t.Fatalf("run state was not rolled back: %#v", stored)
	}
	interactions, err := mem.ListInteractions(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].Status != "pending" || interactions[0].ResolvedAt != nil {
		t.Fatalf("interaction was not rolled back: %#v", interactions)
	}
}

func TestReconcileDurableRunsStartsQueuedAndFailsOrphanedActiveRun(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	queued := &agentcore.AgentRun{
		ID: "queued-1", AppID: "app-a", AgentID: "agent-1",
		Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"}, RuntimeKind: agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeDurable, Status: agentcore.RunStatusQueued,
	}
	running := &agentcore.AgentRun{
		ID: "running-1", AppID: "app-b", AgentID: "agent-2",
		Target: agentcore.TargetRef{Type: "ticket", ID: "T-2"}, RuntimeKind: agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeDurable, Status: agentcore.RunStatusRunning,
	}
	if err := mem.CreateRun(ctx, queued); err != nil {
		t.Fatalf("create queued run: %v", err)
	}
	if err := mem.CreateRun(ctx, running); err != nil {
		t.Fatalf("create running run: %v", err)
	}
	durable := &recordingDurableExecutor{inspectState: DurableExecutionMissing}
	eng := New(Config{Store: mem, Durable: durable})
	count, err := eng.ReconcileDurableRuns(ctx, time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatalf("reconcile durable runs: %v", err)
	}
	if count != 2 || durable.startCalls != 1 {
		t.Fatalf("expected two reconciliations and one workflow start, count=%d starts=%d", count, durable.startCalls)
	}
	stored, err := mem.GetRun(ctx, "app-b", running.ID)
	if err != nil {
		t.Fatalf("get running run: %v", err)
	}
	if stored.Status != agentcore.RunStatusFailed || !strings.Contains(stored.ErrorMessage, "temporal workflow is missing") {
		t.Fatalf("expected orphaned active run to fail, got %#v", stored)
	}
}

func TestExecuteRunOnceResolvesSkillInstructionsAndPolicy(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.Skills = []agentcore.SkillRef{{Key: "approval_protocol"}}
	agent.AllowedTools = []string{"request_approval"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "summarize"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adapter := &recordingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills: skills.NewRegistry(skills.Definition{
			Key:               "approval_protocol",
			Description:       "Approval protocol",
			Instructions:      "Always request approval.",
			RequiredTools:     []string{"request_approval"},
			SupportedRuntimes: []string{agentcore.RuntimeNativeSDK},
			Policy: skills.Policy{
				CompletionRequiresInteractionKinds: []string{skills.InteractionKindApprovalRequest},
			},
		}),
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if adapter.skillInstructions != "Always request approval." {
		t.Fatalf("expected skill instructions in execution context, got %q", adapter.skillInstructions)
	}
	if len(adapter.skillRefs) != 1 || adapter.skillRefs[0].Key != "approval_protocol" {
		t.Fatalf("unexpected skill refs: %#v", adapter.skillRefs)
	}
	if got := adapter.skillPolicy.CompletionRequiresInteractionKinds; len(got) != 1 || got[0] != skills.InteractionKindApprovalRequest {
		t.Fatalf("unexpected skill policy: %#v", adapter.skillPolicy)
	}
}

func TestExecuteRunOnceRejectsSkillRequiredToolMissing(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.Skills = []agentcore.SkillRef{{Key: "approval_protocol"}}
	agent.AllowedTools = []string{"get_context"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "summarize"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adapter := &recordingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills: skills.NewRegistry(skills.Definition{
			Key:           "approval_protocol",
			Description:   "Approval protocol",
			Instructions:  "Always request approval.",
			RequiredTools: []string{"request_approval"},
		}),
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err == nil {
		t.Fatal("expected missing required tool error")
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter executed despite invalid skill requirements")
	}
}

func TestExecuteRunOnceSelectsActiveNativeSkillsBeforeToolValidation(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.ExecutionConfig = json.RawMessage(`{"preset_key":"epic_planner"}`)
	agent.Skills = []agentcore.SkillRef{
		{Key: "approval_protocol"},
		{Key: "prd_authorship"},
		{Key: "task_decomposition"},
		{Key: "epic_state_routing"},
	}
	agent.AllowedTools = []string{"publish_prd_draft"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "epic", ID: "E-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input: agentcore.RunInput{
			Instructions: "plan",
			Metadata:     map[string]interface{}{"planning_stage": skills.PlanningStageDraftSpec},
		},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adapter := &recordingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills: skills.NewRegistry(
			skills.Definition{Key: "approval_protocol", SourceKind: skills.SourceBuiltIn, Instructions: "approval"},
			skills.Definition{Key: "prd_authorship", SourceKind: skills.SourceBuiltIn, Instructions: "prd", RequiredTools: []string{"publish_prd_draft"}},
			skills.Definition{Key: "task_decomposition", SourceKind: skills.SourceBuiltIn, Instructions: "tasks", RequiredTools: []string{"publish_task_plan"}},
			skills.Definition{Key: "epic_state_routing", SourceKind: skills.SourceBuiltIn, Instructions: "routing"},
		),
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if got := skillKeys(adapter.skillRefs); strings.Join(got, ",") != "approval_protocol,prd_authorship,epic_state_routing" {
		t.Fatalf("unexpected active skill refs %#v", got)
	}
	if strings.Contains(adapter.skillInstructions, "tasks") {
		t.Fatalf("inactive task skill instructions leaked: %q", adapter.skillInstructions)
	}
}

func TestExecuteRunOnceStagesSkillsIntoWorkspace(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.Skills = []agentcore.SkillRef{{Key: "code_builder"}}
	agent.ExecutionConfig = json.RawMessage(`{"workspace":{"mode":"host_prepared"}}`)
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		RuntimeKind:   agentcore.RuntimeCodex,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "build"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{lease: agentcore.WorkspaceLease{
		ID:            "lease-1",
		Provider:      "test",
		RootPath:      t.TempDir(),
		CleanupPolicy: workspace.CleanupManual,
	}}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	adapter := &recordingRuntimeAdapter{kind: agentcore.RuntimeCodex}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills:               skills.NewDefaultRegistry(),
		Workspaces:           workspaces,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if adapter.stagedSkillRoot == "" {
		t.Fatal("expected staged skill root")
	}
	if _, err := os.Stat(filepath.Join(adapter.stagedSkillRoot, "01-code_builder", "SKILL.md")); err != nil {
		t.Fatalf("expected staged code_builder skill: %v", err)
	}
	artifacts, err := mem.ListArtifacts(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	foundManifest := false
	for _, artifact := range artifacts {
		if artifact.ArtifactType == "runtime_skill_manifest" {
			foundManifest = true
		}
	}
	if !foundManifest {
		t.Fatalf("expected runtime skill manifest artifact, got %#v", artifacts)
	}
}

func TestExecuteRunOnceStagesSkillsWithoutWorkspaceLease(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.Skills = []agentcore.SkillRef{{Key: "code_builder"}}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "message_generation_task", ID: "task-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "answer"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adapter := &recordingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills:               skills.NewDefaultRegistry(),
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if adapter.stagedSkillRoot == "" {
		t.Fatal("expected staged skill root")
	}
	if !strings.Contains(adapter.stagedSkillRoot, filepath.Join("agent-runtime-skills", run.ID, "skills")) {
		t.Fatalf("expected temp staged skill root, got %q", adapter.stagedSkillRoot)
	}
	if _, err := os.Stat(filepath.Join(adapter.stagedSkillRoot, "01-code_builder", "SKILL.md")); err != nil {
		t.Fatalf("expected staged code_builder skill: %v", err)
	}
}

func TestExecuteRunOnceSkipsWorkspaceForOrdinaryAgent(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "summarize"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{}
	adapter := &recordingRuntimeAdapter{}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Workspaces:           workspaces,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if provider.prepareCalls != 0 {
		t.Fatalf("ordinary run prepared workspace %d times", provider.prepareCalls)
	}
	if adapter.lease != nil {
		t.Fatalf("ordinary run received workspace lease: %#v", adapter.lease)
	}
}

func TestExecuteRunOnceUsesHostPreparedWorkspace(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.AllowedTargets = []string{"repository"}
	agent.ExecutionConfig = json.RawMessage(`{"workspace":{"mode":"host_prepared"}}`)
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "change code"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{lease: agentcore.WorkspaceLease{
		ID:            "lease-1",
		Provider:      "test",
		RootPath:      "/tmp/repo",
		CleanupPolicy: workspace.CleanupOnTerminal,
	}, finalizeSummary: json.RawMessage(`{"repository":{"changed":true,"branch":"agent/run-1"}}`)}
	adapter := &recordingRuntimeAdapter{outputSummary: json.RawMessage(`{"input_tokens":2,"output_tokens":3}`)}
	events := &recordingEngineEventSink{}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Workspaces:           workspaces,
		EventSink:            events,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if provider.prepareCalls != 1 || provider.finalizeCalls != 1 || provider.cleanupCalls != 1 {
		t.Fatalf("unexpected workspace lifecycle: prepare=%d finalize=%d cleanup=%d", provider.prepareCalls, provider.finalizeCalls, provider.cleanupCalls)
	}
	if adapter.lease == nil || adapter.lease.ID != "lease-1" {
		t.Fatalf("adapter did not receive workspace lease: %#v", adapter.lease)
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.WorkspaceLease == nil || stored.WorkspaceLease.ID != "lease-1" {
		t.Fatalf("workspace lease was not persisted: %#v", stored.WorkspaceLease)
	}
	if provider.finalizeOutcome != agentcore.RunStatusCompleted {
		t.Fatalf("finalize outcome = %q", provider.finalizeOutcome)
	}
	if !strings.Contains(string(stored.OutputSummary), `"input_tokens":2`) || !strings.Contains(string(stored.OutputSummary), `"repository"`) {
		t.Fatalf("expected merged usage and repository output summary, got %s", string(stored.OutputSummary))
	}
	completed := waitForEventType(t, events, "run.completed")
	if got := usageTotalFromEvent(t, completed); got != 5 {
		t.Fatalf("expected completed usage after workspace finalize, got %d in %#v", got, completed.Data)
	}
}

func TestExecuteRunOnceUsesRepositoryWorkspaceMode(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.AllowedTargets = []string{"repository"}
	agent.ExecutionConfig = json.RawMessage(`{"workspace":{"mode":"repository"}}`)
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "change code"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{lease: agentcore.WorkspaceLease{
		ID:            "lease-1",
		Provider:      "repository",
		RootPath:      "/tmp/repo",
		CleanupPolicy: workspace.CleanupOnTerminal,
	}}
	adapter := &recordingRuntimeAdapter{}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Workspaces:           workspaces,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if provider.prepareCalls != 1 || provider.prepareRequest.WorkspaceMode != workspace.ModeRepository {
		t.Fatalf("expected repository workspace prepare, calls=%d req=%#v", provider.prepareCalls, provider.prepareRequest)
	}
	if adapter.lease == nil || adapter.lease.Provider != "repository" {
		t.Fatalf("adapter did not receive repository lease: %#v", adapter.lease)
	}
}

func TestExecuteRunOnceUsesRunMetadataRepositoryWorkspaceMode(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.AllowedTargets = []string{"task"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "task", ID: "task-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input: agentcore.RunInput{
			Instructions: "plan task",
			Metadata: map[string]interface{}{
				"workspace_mode": "repository",
				"repository_id":  "repo-1",
			},
		},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{lease: agentcore.WorkspaceLease{
		ID:            "lease-1",
		Provider:      "repository",
		RootPath:      "/tmp/repo",
		CleanupPolicy: workspace.CleanupOnTerminal,
	}}
	adapter := &recordingRuntimeAdapter{}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Workspaces:           workspaces,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if provider.prepareCalls != 1 || provider.prepareRequest.WorkspaceMode != workspace.ModeRepository {
		t.Fatalf("expected repository workspace prepare from run metadata, calls=%d req=%#v", provider.prepareCalls, provider.prepareRequest)
	}
	if adapter.lease == nil || adapter.lease.Provider != "repository" {
		t.Fatalf("adapter did not receive repository lease: %#v", adapter.lease)
	}
}

func TestPrepareRunOnceUsesRunMetadataRepositoryWorkspaceMode(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.AllowedTargets = []string{"task"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "task", ID: "task-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeDurable,
		Status:        agentcore.RunStatusQueued,
		Input: agentcore.RunInput{
			Instructions: "plan task",
			Metadata: map[string]interface{}{
				"workspace_mode": "repository",
				"repository_id":  "repo-1",
			},
		},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{lease: agentcore.WorkspaceLease{
		ID:            "lease-1",
		Provider:      "repository",
		RootPath:      "/tmp/repo",
		CleanupPolicy: workspace.CleanupOnTerminal,
	}}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(&recordingRuntimeAdapter{}),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Workspaces:           workspaces,
	})

	if err := eng.PrepareRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("prepare run: %v", err)
	}
	if provider.prepareCalls != 1 || provider.prepareRequest.WorkspaceMode != workspace.ModeRepository {
		t.Fatalf("expected repository workspace prepare from run metadata, calls=%d req=%#v", provider.prepareCalls, provider.prepareRequest)
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.WorkspaceLease == nil || stored.WorkspaceLease.Provider != "repository" {
		t.Fatalf("workspace lease was not persisted: %#v", stored.WorkspaceLease)
	}
	if stored.Status != agentcore.RunStatusRunning {
		t.Fatalf("status = %q, want running", stored.Status)
	}
}

func TestExecuteRunOnceRepreparesInvalidRepositoryLease(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.AllowedTargets = []string{"repository"}
	agent.ExecutionConfig = json.RawMessage(`{"workspace":{"mode":"repository"}}`)
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "change code"},
		WorkspaceLease: &agentcore.WorkspaceLease{
			ID:       "stale-lease",
			Provider: "repository",
			RootPath: "/tmp/wrong-repo",
		},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{
		validateValid: false,
		lease: agentcore.WorkspaceLease{
			ID:            "fresh-lease",
			Provider:      "repository",
			RootPath:      "/tmp/right-repo",
			CleanupPolicy: workspace.CleanupOnTerminal,
		},
	}
	adapter := &recordingRuntimeAdapter{}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Workspaces:           workspaces,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if provider.validateCalls != 1 {
		t.Fatalf("expected one repository lease validation, got %d", provider.validateCalls)
	}
	if provider.prepareCalls != 1 {
		t.Fatalf("expected invalid lease to be reprepared, prepare calls=%d", provider.prepareCalls)
	}
	if adapter.lease == nil || adapter.lease.ID != "fresh-lease" || adapter.lease.RootPath != "/tmp/right-repo" {
		t.Fatalf("adapter received wrong lease: %#v", adapter.lease)
	}
}

func TestExecuteRunOnceDoesNotOverwriteCancelledRunAfterAdapterReturns(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	adapter := newBlockingRuntimeAdapter()
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
	})
	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Instructions: "do work",
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("adapter did not start")
	}
	if _, err := eng.CancelRun(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	close(adapter.release)
	select {
	case <-adapter.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("adapter did not finish")
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.Status != agentcore.RunStatusCancelled {
		t.Fatalf("expected cancelled run to stay cancelled, got %s", stored.Status)
	}
}

func TestCancelRunResolvesHostRunID(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:            "run-runtime-1",
		AppID:         "app-a",
		HostRunID:     "helpin-run-1",
		AgentID:       "agent-1",
		Target:        agentcore.TargetRef{Type: "task", ID: "task-1"},
		ExecutionMode: ExecutionModeLightweight,
		Status:        agentcore.RunStatusRunning,
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	eng := New(Config{Store: mem})

	cancelled, err := eng.CancelRun(ctx, "app-a", "helpin-run-1")
	if err != nil {
		t.Fatalf("cancel run by host id: %v", err)
	}
	if cancelled.ID != "run-runtime-1" || cancelled.Status != agentcore.RunStatusCancelled {
		t.Fatalf("unexpected cancelled run: %#v", cancelled)
	}
	stored, err := mem.GetRun(ctx, "app-a", "run-runtime-1")
	if err != nil {
		t.Fatalf("get runtime run: %v", err)
	}
	if stored == nil || stored.Status != agentcore.RunStatusCancelled {
		t.Fatalf("expected runtime run to be cancelled, got %#v", stored)
	}
}

type recordingRuntimeAdapter struct {
	kind              string
	lease             *agentcore.WorkspaceLease
	calls             int
	skillRefs         []agentcore.SkillRef
	skillInstructions string
	skillPolicy       skills.Policy
	stagedSkillRoot   string
	outputSummary     json.RawMessage
	toolCalls         []agentcore.ToolCall
}

func (a *recordingRuntimeAdapter) Kind() string {
	if a.kind != "" {
		return a.kind
	}
	return agentcore.RuntimeNativeSDK
}

func (a *recordingRuntimeAdapter) Execute(execCtx *runtime.ExecutionContext) (*runtime.Result, error) {
	a.calls++
	a.lease = execCtx.WorkspaceLease
	a.skillRefs = append([]agentcore.SkillRef(nil), execCtx.SkillRefs...)
	a.skillInstructions = execCtx.SkillInstructions
	a.skillPolicy = execCtx.SkillPolicy
	a.stagedSkillRoot = execCtx.StagedSkillRoot
	for _, configured := range a.toolCalls {
		call := configured
		call.AppID = execCtx.Run.AppID
		call.RunID = execCtx.Run.ID
		if err := execCtx.Store.AppendToolCall(execCtx.Context, &call); err != nil {
			return nil, err
		}
	}
	summary := a.outputSummary
	if len(summary) == 0 {
		summary = json.RawMessage(`{"ok":true}`)
	}
	return &runtime.Result{
		AssistantMessage: "done",
		OutputSummary:    summary,
	}, nil
}

type blockingRuntimeAdapter struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func newBlockingRuntimeAdapter() *blockingRuntimeAdapter {
	return &blockingRuntimeAdapter{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
}

func (a *blockingRuntimeAdapter) Kind() string {
	return agentcore.RuntimeNativeSDK
}

func (a *blockingRuntimeAdapter) Execute(_ *runtime.ExecutionContext) (*runtime.Result, error) {
	close(a.started)
	<-a.release
	defer close(a.finished)
	return &runtime.Result{
		AssistantMessage: "done after cancel",
		OutputSummary:    json.RawMessage(`{"ok":true}`),
	}, nil
}

type recordingDurableExecutor struct {
	store        agentcore.Store
	startCalls   int
	startErr     error
	resumeCalls  int
	resumeErr    error
	runAtResume  *agentcore.AgentRun
	inspectState string
	inspectErr   error
}

func (d *recordingDurableExecutor) StartRun(ctx context.Context, run *agentcore.AgentRun) error {
	d.startCalls++
	return d.startErr
}

func (d *recordingDurableExecutor) CancelRun(ctx context.Context, run *agentcore.AgentRun) error {
	return nil
}

func (d *recordingDurableExecutor) ResumeRun(ctx context.Context, run *agentcore.AgentRun, payload ResumePayload) error {
	d.resumeCalls++
	if d.resumeErr != nil {
		return d.resumeErr
	}
	if d.store != nil {
		stored, _ := d.store.GetRun(ctx, run.AppID, run.ID)
		d.runAtResume = stored
		return nil
	}
	cp := *run
	d.runAtResume = &cp
	return nil
}

func (d *recordingDurableExecutor) InspectRun(context.Context, *agentcore.AgentRun) (string, error) {
	if d.inspectErr != nil {
		return "", d.inspectErr
	}
	if d.inspectState == "" {
		return DurableExecutionRunning, nil
	}
	return d.inspectState, nil
}

func skillKeys(refs []agentcore.SkillRef) []string {
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, ref.Key)
	}
	return keys
}

type pausingRuntimeAdapter struct {
	calls int
}

func (a *pausingRuntimeAdapter) Kind() string {
	return agentcore.RuntimeNativeSDK
}

func (a *pausingRuntimeAdapter) Execute(execCtx *runtime.ExecutionContext) (*runtime.Result, error) {
	a.calls++
	if a.calls == 1 {
		_ = execCtx.InteractionBroker.RequestInteraction(execCtx.Context, agentcore.AgentRunInteraction{
			InteractionKind: "request_user_input",
			Status:          "pending",
			Title:           "Input requested",
			Summary:         "Who owns this?",
			RequestPayload:  json.RawMessage(`{"request_schema":"request_user_input_v1"}`),
		})
		return &runtime.Result{
			AssistantMessage: "Need input.",
			OutputSummary:    json.RawMessage(`{"native_messages":[{"role":"user","content":"ask"}]}`),
			AwaitingInput:    true,
		}, nil
	}
	return &runtime.Result{
		AssistantMessage: "Done.",
		OutputSummary:    json.RawMessage(`{"ok":true}`),
	}, nil
}

type recordingWorkspaceProvider struct {
	lease           agentcore.WorkspaceLease
	prepareCalls    int
	validateCalls   int
	validateValid   bool
	validateLease   *agentcore.WorkspaceLease
	finalizeCalls   int
	cleanupCalls    int
	finalizeOutcome string
	prepareRequest  workspace.PrepareRequest
	finalizeSummary json.RawMessage
}

func (p *recordingWorkspaceProvider) PrepareWorkspace(_ context.Context, req workspace.PrepareRequest) (*agentcore.WorkspaceLease, error) {
	p.prepareCalls++
	p.prepareRequest = req
	lease := p.lease
	if lease.ID == "" {
		lease = agentcore.WorkspaceLease{ID: "lease-1", RootPath: "/tmp/repo"}
	}
	return &lease, nil
}

func (p *recordingWorkspaceProvider) ValidateWorkspace(_ context.Context, _ workspace.PrepareRequest, lease agentcore.WorkspaceLease) (*agentcore.WorkspaceLease, bool, error) {
	p.validateCalls++
	if p.validateLease != nil {
		return p.validateLease, p.validateValid, nil
	}
	return &lease, p.validateValid, nil
}

func (p *recordingWorkspaceProvider) FinalizeWorkspace(_ context.Context, req workspace.FinalizeRequest) (*workspace.FinalizeResult, error) {
	p.finalizeCalls++
	p.finalizeOutcome = req.Outcome
	return &workspace.FinalizeResult{OutputSummary: p.finalizeSummary}, nil
}

func (p *recordingWorkspaceProvider) CleanupWorkspace(_ context.Context, _ workspace.CleanupRequest) error {
	p.cleanupCalls++
	return nil
}

func testAgent(appID string) agentcore.Agent {
	return agentcore.Agent{
		AppID:                 appID,
		Name:                  "Test Agent",
		RuntimeKind:           agentcore.RuntimeNativeSDK,
		AllowedTools:          []string{"get_context"},
		AllowedTargets:        []string{"ticket"},
		ApprovalMode:          agentcore.ApprovalModeNever,
		DefaultInvocationMode: agentcore.InvocationAutonomous,
	}
}

func testEngine(mem *store.Memory, targets host.TargetContextProvider) *Engine {
	return New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(runtime.NewNativeAdapter(), runtime.NewCodexAdapter()),
		Tools:                tools.NewRegistry(),
		Targets:              targets,
	})
}

func waitForRunStatus(t *testing.T, mem *store.Memory, appID, runID, status string) *agentcore.AgentRun {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := mem.GetRun(context.Background(), appID, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run != nil && run.Status == status {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, _ := mem.GetRun(context.Background(), appID, runID)
	t.Fatalf("run did not reach status %q; got %#v", status, run)
	return nil
}

type recordingEngineEventSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *recordingEngineEventSink) Emit(_ context.Context, event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.Data != nil {
		cp := map[string]interface{}{}
		for key, value := range event.Data {
			cp[key] = value
		}
		event.Data = cp
	}
	s.events = append(s.events, event)
}

func (s *recordingEngineEventSink) snapshot() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

func waitForEventType(t *testing.T, sink *recordingEngineEventSink, eventType string) Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range sink.snapshot() {
			if event.Type == eventType {
				return event
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for event %q; got %#v", eventType, sink.snapshot())
	return Event{}
}

func usageTotalFromEvent(t *testing.T, event Event) int64 {
	t.Helper()
	raw := event.Data["usage"]
	switch usage := raw.(type) {
	case agentcore.Usage:
		return usage.TotalTokens
	case map[string]interface{}:
		value, _ := usage["total_tokens"].(float64)
		return int64(value)
	default:
		t.Fatalf("unexpected usage payload %T %#v", raw, raw)
		return 0
	}
}
