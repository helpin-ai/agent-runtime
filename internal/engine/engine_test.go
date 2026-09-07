package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
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

func TestStartRunDurableDispatchFailureClearsMCPCredential(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	targets := host.NewStaticContextProvider()
	targets.Register("app-a", agentcore.TargetRef{Type: "ticket", ID: "T-1"}, host.TargetContext{Summary: "ticket context"})
	durable := &recordingDurableExecutor{startErr: errors.New("temporal unavailable")}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeDurable,
		Store:                mem,
		Durable:              durable,
		Targets:              targets,
		Tools:                tools.NewRegistry(),
		RunMCP:               mcp.RunConfig{CredentialKey: []byte("0123456789abcdef0123456789abcdef")},
	})

	_, err := eng.StartRun(ctx, StartRunRequest{
		AppID: "app-a", HostRunID: "host-durable-failure", AgentID: agent.ID,
		Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		MCPServers: []mcp.RunServerRequest{{
			ServerID: "github-1", ServerName: "github", Transport: agentcore.MCPTransportStreamableHTTP,
			URL: "https://mcp.example.com/mcp", Tools: []mcp.RunTool{{Name: "get_issue", Access: agentcore.MCPToolAccessRead}},
			Credential: &mcp.RunCredential{Type: mcp.CredentialBearerToken, AccessToken: "run-secret"},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "temporal unavailable") {
		t.Fatalf("expected durable dispatch error, got %v", err)
	}
	stored, err := mem.GetRunByHostRunID(ctx, "app-a", "host-durable-failure")
	if err != nil || stored == nil {
		t.Fatalf("get failed run: run=%#v err=%v", stored, err)
	}
	if stored.Status != agentcore.RunStatusFailed {
		t.Fatalf("run status = %q, want failed", stored.Status)
	}
	servers, err := mem.ListRunMCPServers(ctx, "app-a", stored.ID)
	if err != nil || len(servers) != 1 {
		t.Fatalf("list run MCP servers: servers=%#v err=%v", servers, err)
	}
	if len(servers[0].EncryptedCredential) != 0 {
		t.Fatal("durable dispatch failure retained the run MCP credential")
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

func TestRunCompletionRequiresRunScopedToolCall(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.AllowedTargets = []string{"support_coverage_gap"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	targets := host.NewStaticContextProvider()
	targets.Register("app-a", agentcore.TargetRef{Type: "support_coverage_gap", ID: "gap-1"}, host.TargetContext{Summary: "gap context"})
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(&recordingRuntimeAdapter{}),
		Tools:                tools.NewRegistry(),
		Targets:              targets,
	})
	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID: "app-a", AgentID: agent.ID,
		Target: agentcore.TargetRef{Type: "support_coverage_gap", ID: "gap-1"},
		Metadata: map[string]interface{}{
			"completion_required_tools": []string{"complete_support_coverage_gap"},
		},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	stored := waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusFailed)
	if !strings.Contains(stored.ErrorMessage, "complete_support_coverage_gap") {
		t.Fatalf("expected run-scoped completion error, got %q", stored.ErrorMessage)
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
	if got := usageTotalFromEvent(t, checkpoint); got != 8 {
		t.Fatalf("expected checkpoint total usage 8, got %d in %#v", got, checkpoint.Data)
	}
	completed := waitForEventType(t, events, "run.completed")
	if completed.HostRunID != "helpin-run-123" || completed.Data["host_run_id"] != "helpin-run-123" {
		t.Fatalf("completed event did not include host run id: %#v", completed)
	}
	if got := usageTotalFromEvent(t, completed); got != 8 {
		t.Fatalf("expected completed total usage 8, got %d in %#v", got, completed.Data)
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
	if usage.InputTokens != 7 || usage.CachedInputTokens != 1 || usage.OutputTokens != 10 || usage.TotalTokens != 17 {
		t.Fatalf("expected cumulative native usage, got summary=%s usage=%#v", string(summary), usage)
	}

	codex := cumulativeOutputSummary(base, current, agentcore.RuntimeCodex)
	if string(codex) != string(current) {
		t.Fatalf("expected codex summary to remain adapter cumulative value, got %s", string(codex))
	}
}

func TestCumulativeOutputSummaryDoesNotAddNativeCheckpointTwice(t *testing.T) {
	base := json.RawMessage(`{"input_tokens":100,"output_tokens":20}`)
	current := json.RawMessage(`{"input_tokens":120,"output_tokens":30,"usage_semantic":"cumulative"}`)
	summary := cumulativeOutputSummary(base, current, agentcore.RuntimeNativeSDK)
	usage := usageFromSummary(summary)
	if usage.InputTokens != 120 || usage.OutputTokens != 30 {
		t.Fatalf("checkpoint double counted: %s", summary)
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
	toolRegistry := tools.NewRegistry()
	closer := &recordingRunCloser{}
	toolRegistry.RegisterRunCloser(closer)
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                toolRegistry,
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
	if closer.Count() != 0 {
		t.Fatalf("paused run closed tool resources %d times", closer.Count())
	}
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
	if closer.Count() != 1 {
		t.Fatalf("completed run closed tool resources %d times, want 1", closer.Count())
	}
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

	resumePolicy := &agentcore.TurnPolicy{
		Mode:                     agentcore.TurnPolicyPauseAfterAssist,
		CompletionMode:           agentcore.TurnCompletionExplicit,
		MaxCompletionCorrections: 2,
	}
	if _, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{Intent: "reply", Content: "continue", TurnPolicy: resumePolicy}); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if durable.resumeCalls != 1 {
		t.Fatalf("expected durable resume call, got %d", durable.resumeCalls)
	}
	if durable.runAtResume == nil || durable.runAtResume.Input.Metadata["last_resume"] == nil {
		t.Fatalf("expected durable signal after last_resume persisted, got %#v", durable.runAtResume)
	}
	if durable.runAtResume.Input.TurnPolicy.CompletionMode != agentcore.TurnCompletionExplicit || durable.runAtResume.Input.TurnPolicy.MaxCompletionCorrections != 2 {
		t.Fatalf("expected resume policy persisted before durable signal, got %#v", durable.runAtResume.Input.TurnPolicy)
	}
}

func TestExecuteRunOnceExplicitCompletionBackstopRejectsSilentAdapterStop(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		ID:            "run-explicit-backstop",
		AppID:         agent.AppID,
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Status:        agentcore.RunStatusQueued,
		Input: agentcore.RunInput{TurnPolicy: agentcore.TurnPolicy{
			Mode:                     agentcore.TurnPolicyPauseAfterAssist,
			CompletionMode:           agentcore.TurnCompletionExplicit,
			MaxCompletionCorrections: 2,
		}},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	eng := New(Config{
		Store:    mem,
		Runtimes: runtime.NewRegistry(&recordingRuntimeAdapter{}),
		Tools:    tools.NewRegistry(),
		Targets:  host.NewStaticContextProvider(),
	})
	_, err := eng.ExecuteRunOnce(ctx, run.AppID, run.ID)
	if err == nil || !strings.Contains(err.Error(), "turn_completion_guard_exhausted") {
		t.Fatalf("expected explicit completion backstop error, got %v", err)
	}
	stored, getErr := mem.GetRun(ctx, run.AppID, run.ID)
	if getErr != nil {
		t.Fatalf("get run: %v", getErr)
	}
	if stored.Status != agentcore.RunStatusFailed || stored.Status == agentcore.RunStatusPaused {
		t.Fatalf("guard failure must be terminal and never look like a chat pause: %#v", stored)
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

	// The completion policy requires an approval interaction before the run may
	// complete; satisfy it up front so this test stays about policy plumbing.
	if err := mem.AppendInteraction(ctx, &agentcore.AgentRunInteraction{
		AppID:           "app-a",
		RunID:           run.ID,
		InteractionKind: "approval_request",
		Status:          "resolved",
		Title:           "Approved earlier",
	}); err != nil {
		t.Fatalf("seed approval interaction: %v", err)
	}

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

func TestResolveSkillsIncludesRunMCPAttachmentSkills(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		ID: "run-connector-skill", AppID: agent.AppID, AgentID: agent.ID,
		Target: agentcore.TargetRef{Type: "workspace", ID: "ws-1"}, RuntimeKind: agent.RuntimeKind,
	}
	server := agentcore.RunMCPServer{
		ServerID: "customer-io-1", ServerName: "customer_io",
		Tools: []agentcore.RunMCPTool{
			{Name: "cio_skills_list", Access: agentcore.MCPToolAccessRead},
			{Name: "cio_skills_read", Access: agentcore.MCPToolAccessRead},
		},
		Skills: []agentcore.SkillRef{{Key: "customer_io_operator"}},
	}
	if err := mem.CreateRunWithMCP(ctx, run, []agentcore.RunMCPServer{server}); err != nil {
		t.Fatalf("create run with MCP: %v", err)
	}
	registry := skills.NewRegistry(skills.Definition{
		Key:          "customer_io_operator",
		Instructions: "Load the relevant Customer.io provider skill before querying.",
		RequiredTools: []string{
			"mcp__customer_io__cio_skills_list",
			"mcp__customer_io__cio_skills_read",
		},
	})
	eng := New(Config{Store: mem, Skills: registry})
	_, allowedTools, err := eng.skillPolicyForRun(ctx, &agent, run)
	if err != nil {
		t.Fatalf("resolve connector skill policy: %v", err)
	}
	for _, required := range []string{
		"mcp__customer_io__cio_skills_list",
		"mcp__customer_io__cio_skills_read",
	} {
		found := false
		for _, allowed := range allowedTools {
			if allowed == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("run skill policy missing MCP alias %q: %#v", required, allowedTools)
		}
	}
	resolution, err := eng.resolveSkills(ctx, &agent, run)
	if err != nil {
		t.Fatalf("resolve connector skill: %v", err)
	}
	if len(resolution.CoreRefs) != 1 || resolution.CoreRefs[0].Key != "customer_io_operator" {
		t.Fatalf("unexpected connector skill refs: %#v", resolution.CoreRefs)
	}

	plainAgent, err := mem.GetAgent(ctx, agent.AppID, agent.ID)
	if err != nil {
		t.Fatalf("reload saved agent: %v", err)
	}
	plainRun := &agentcore.AgentRun{ID: "run-without-connector", AppID: agent.AppID, AgentID: agent.ID}
	if err := mem.CreateRun(ctx, plainRun); err != nil {
		t.Fatalf("create run without MCP: %v", err)
	}
	resolution, err = eng.resolveSkills(ctx, plainAgent, plainRun)
	if err != nil {
		t.Fatalf("resolve run without connector: %v", err)
	}
	if len(resolution.CoreRefs) != 0 {
		t.Fatalf("connector skill leaked into run without MCP: %#v", resolution.CoreRefs)
	}
}

type completionPolicyTestAdapter struct {
	calls        int
	instructions []string
	onCall       func(call int, execCtx *runtime.ExecutionContext) (*runtime.Result, error)
}

func (a *completionPolicyTestAdapter) Kind() string {
	return agentcore.RuntimeNativeSDK
}

func (a *completionPolicyTestAdapter) Execute(execCtx *runtime.ExecutionContext) (*runtime.Result, error) {
	a.calls++
	a.instructions = append(a.instructions, execCtx.Run.Input.Instructions)
	if a.onCall != nil {
		return a.onCall(a.calls, execCtx)
	}
	return &runtime.Result{AssistantMessage: "done"}, nil
}

func newCompletionPolicyEngine(t *testing.T, mem *store.Memory, adapter runtime.Adapter) *Engine {
	t.Helper()
	return New(Config{
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
}

func newCompletionPolicyRun(t *testing.T, ctx context.Context, mem *store.Memory) *agentcore.AgentRun {
	t.Helper()
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
	return run
}

func TestExecuteRunOnceFailsCompletionWithoutRequiredInteraction(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := newCompletionPolicyRun(t, ctx, mem)
	adapter := &completionPolicyTestAdapter{}
	eng := newCompletionPolicyEngine(t, mem, adapter)

	_, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID)
	if err == nil || !strings.Contains(err.Error(), "require one of [approval_request]") {
		t.Fatalf("expected completion interaction policy error, got %v", err)
	}
	if adapter.calls != 2 {
		t.Fatalf("expected one corrective retry before failing, got %d calls", adapter.calls)
	}
	if len(adapter.instructions) != 2 || !strings.Contains(adapter.instructions[1], "request_approval") {
		t.Fatalf("expected corrective instructions on the retry, got %#v", adapter.instructions)
	}
	stored, getErr := mem.GetRun(ctx, "app-a", run.ID)
	if getErr != nil || stored == nil {
		t.Fatalf("load run: %v", getErr)
	}
	if stored.Status != agentcore.RunStatusFailed {
		t.Fatalf("expected failed run, got %q", stored.Status)
	}
	if stored.Input.Instructions != "summarize" {
		t.Fatalf("corrective instructions leaked into the stored run: %q", stored.Input.Instructions)
	}
}

func TestExecuteRunOnceCompletionPolicyCorrectiveTurnPauses(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := newCompletionPolicyRun(t, ctx, mem)
	adapter := &completionPolicyTestAdapter{
		onCall: func(call int, execCtx *runtime.ExecutionContext) (*runtime.Result, error) {
			if call < 2 {
				return &runtime.Result{AssistantMessage: "draft published"}, nil
			}
			if err := execCtx.InteractionBroker.RequestInteraction(execCtx.Context, agentcore.AgentRunInteraction{
				InteractionKind: "approval_request",
				Status:          "pending",
				Title:           "Approve the draft",
			}); err != nil {
				return nil, err
			}
			return &runtime.Result{AssistantMessage: "approval requested", WaitForApproval: true}, nil
		},
	}
	eng := newCompletionPolicyEngine(t, mem, adapter)

	result, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if adapter.calls != 2 {
		t.Fatalf("expected corrective retry, got %d calls", adapter.calls)
	}
	if result == nil || !result.WaitForApproval {
		t.Fatalf("expected paused result after corrective turn, got %#v", result)
	}
	stored, getErr := mem.GetRun(ctx, "app-a", run.ID)
	if getErr != nil || stored == nil {
		t.Fatalf("load run: %v", getErr)
	}
	if stored.Status != agentcore.RunStatusPaused || stored.PauseReason != agentcore.PauseReasonHumanApproval {
		t.Fatalf("expected paused run awaiting approval, got status=%q reason=%q", stored.Status, stored.PauseReason)
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

	// publish_prd_draft makes the run approval-gated; satisfy the completion
	// interaction policy up front so this test stays about skill selection.
	if err := mem.AppendInteraction(ctx, &agentcore.AgentRunInteraction{
		AppID:           "app-a",
		RunID:           run.ID,
		InteractionKind: "approval_request",
		Status:          "resolved",
		Title:           "Approved earlier",
	}); err != nil {
		t.Fatalf("seed approval interaction: %v", err)
	}

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

func TestExecuteRunOnceSplitContractStagesAvailableSkillWithoutInjectingPromptText(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.SystemPrompt = "One complete version-owned prompt."
	agent.Skills = []agentcore.SkillRef{{
		Key:    "general_agent_behavior",
		Config: json.RawMessage(`{"runtime_skill_role":"available"}`),
	}}
	agent.ExecutionConfig = json.RawMessage(`{"workspace":{"mode":"host_prepared"},"runtime_policy":{"completion_requires_interaction_kinds":["approval_request"]}}`)
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "inspect"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := mem.AppendInteraction(ctx, &agentcore.AgentRunInteraction{
		AppID:           "app-a",
		RunID:           run.ID,
		InteractionKind: skills.InteractionKindApprovalRequest,
		Status:          "resolved",
		Title:           "Approved",
	}); err != nil {
		t.Fatalf("seed approval interaction: %v", err)
	}
	provider := &recordingWorkspaceProvider{lease: agentcore.WorkspaceLease{
		ID:            "lease-split",
		Provider:      "test",
		RootPath:      t.TempDir(),
		CleanupPolicy: workspace.CleanupManual,
	}}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	adapter := &recordingRuntimeAdapter{}
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
	if !adapter.usesSplitSkills || len(adapter.skillRefs) != 0 || adapter.skillInstructions != "" {
		t.Fatalf("available skill leaked into active prompt state: split=%v refs=%#v instructions=%q", adapter.usesSplitSkills, adapter.skillRefs, adapter.skillInstructions)
	}
	if len(adapter.availableSkillRefs) != 1 || adapter.availableSkillRefs[0].Key != "general_agent_behavior" {
		t.Fatalf("unexpected available skill refs: %#v", adapter.availableSkillRefs)
	}
	if got := adapter.skillPolicy.CompletionRequiresInteractionKinds; len(got) != 1 || got[0] != skills.InteractionKindApprovalRequest {
		t.Fatalf("version-owned runtime policy was not preserved: %#v", adapter.skillPolicy)
	}
	if _, err := os.Stat(filepath.Join(adapter.stagedSkillRoot, "01-general_agent_behavior", "SKILL.md")); err != nil {
		t.Fatalf("expected staged available skill: %v", err)
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

func TestWorkspaceManagerMarksDynamicPrimaryRepositoryForResume(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "task", ID: "task-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeDurable,
		Input:         agentcore.RunInput{Instructions: "inspect the repository"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{lease: agentcore.WorkspaceLease{
		ID:            "dynamic-lease",
		Provider:      "host-repository",
		RootPath:      t.TempDir(),
		CleanupPolicy: workspace.CleanupOnTerminal,
	}}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	eng := New(Config{Store: mem, Workspaces: workspaces})
	manager := engineWorkspaceManager{engine: eng, agent: &agent, run: run}
	result, err := manager.CheckoutRepository(ctx, tools.CheckoutRepositoryRequest{
		RepositoryID: "repo-1",
		Alias:        "primary",
	})
	if err != nil {
		t.Fatalf("checkout repository: %v", err)
	}
	if result == nil || !result.Primary {
		t.Fatalf("dynamic checkout was not primary: %#v", result)
	}
	stored, err := mem.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if mode := runWorkspaceMode(stored); mode != workspace.ModeRepository {
		t.Fatalf("workspace mode = %q, want repository", mode)
	}
	if stored.Input.Metadata["repository_id"] != "repo-1" || stored.Input.Metadata["repo_alias"] != "primary" {
		t.Fatalf("dynamic repository selection was not persisted: %#v", stored.Input.Metadata)
	}
	if stored.WorkspaceLease == nil || stored.WorkspaceLease.Metadata["workspace_mode"] != workspace.ModeRepository {
		t.Fatalf("workspace lease was not marked for resume: %#v", stored.WorkspaceLease)
	}
}

func TestExecuteRunOnceReusesValidDynamicRepositoryLease(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "task", ID: "task-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "continue after input"},
		WorkspaceLease: &agentcore.WorkspaceLease{
			ID:            "existing-lease",
			Provider:      "host-repository",
			RootPath:      "/tmp/existing-repo",
			CleanupPolicy: workspace.CleanupOnTerminal,
			Metadata:      map[string]interface{}{"workspace_mode": workspace.ModeRepository},
		},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{validateValid: true}
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

	if _, err := eng.ExecuteRunOnce(ctx, run.AppID, run.ID); err != nil {
		t.Fatalf("execute resumed run: %v", err)
	}
	if provider.validateCalls != 1 || provider.prepareCalls != 0 {
		t.Fatalf("expected valid checkout reuse, validate=%d prepare=%d", provider.validateCalls, provider.prepareCalls)
	}
	if adapter.lease == nil || adapter.lease.ID != "existing-lease" {
		t.Fatalf("adapter did not receive existing checkout: %#v", adapter.lease)
	}
}

func TestExecuteRunOnceRepreparesInvalidDynamicRepositoryLease(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "task", ID: "task-1"},
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

func TestExecuteRunOnceLeavesInterruptedRunRetryable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:          "app-a",
		AgentID:        agent.ID,
		Target:         agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:    agentcore.RuntimeNativeSDK,
		ExecutionMode:  ExecutionModeDurable,
		InvocationMode: agentcore.InvocationAutonomous,
		Status:         agentcore.RunStatusQueued,
		PauseReason:    agentcore.PauseReasonNone,
		ApprovalState:  agentcore.ApprovalNotRequired,
	}
	if err := mem.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	events := &recordingEngineEventSink{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeDurable,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(&interruptingRuntimeAdapter{cancel: cancel}),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		EventSink:            events,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	stored, err := mem.GetRun(context.Background(), "app-a", run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.Status != agentcore.RunStatusRunning || stored.CompletedAt != nil || stored.ErrorMessage != "" {
		t.Fatalf("interrupted run became terminal: %#v", stored)
	}
	for _, event := range events.snapshot() {
		if event.Type == "run.failed" || event.Type == "workspace.finalized" || event.Type == "workspace.cleaned" {
			t.Fatalf("interruption emitted terminal event: %#v", event)
		}
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

func TestCancelRunClearsEncryptedMCPCredentials(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID: "run-mcp", AppID: "app-a", AgentID: "agent-1",
		Target: agentcore.TargetRef{Type: "workspace", ID: "ws-1"}, ExecutionMode: ExecutionModeLightweight, Status: agentcore.RunStatusRunning,
	}
	if err := mem.CreateRunWithMCP(ctx, run, []agentcore.RunMCPServer{{
		ServerID: "server-1", ServerName: "github", Transport: agentcore.MCPTransportStreamableHTTP,
		URL: "https://mcp.example.com/mcp", Tools: []agentcore.RunMCPTool{{Name: "get_issue", Access: agentcore.MCPToolAccessRead}},
		EncryptedCredential: []byte{1, 2, 3},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Store: mem}).CancelRun(ctx, "app-a", run.ID); err != nil {
		t.Fatal(err)
	}
	servers, err := mem.ListRunMCPServers(ctx, "app-a", run.ID)
	if err != nil || len(servers) != 1 || len(servers[0].EncryptedCredential) != 0 {
		t.Fatalf("terminal credential was not cleared: %#v err=%v", servers, err)
	}
}

func TestExecuteRunPausesWhenRunMCPReturnsUnauthorized(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer remote.Close()
	ctx := context.Background()
	mem := store.NewMemory()
	agent := &agentcore.Agent{
		ID: "agent-mcp-auth", AppID: "app-a", Name: "MCP auth agent",
		RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTargets: []string{"workspace"},
	}
	if err := mem.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	run := &agentcore.AgentRun{
		ID: "run-mcp-auth", AppID: "app-a", AgentID: agent.ID,
		RuntimeKind: agentcore.RuntimeNativeSDK,
		Target:      agentcore.TargetRef{Type: "workspace", ID: "ws-1"},
	}
	servers, err := mcp.PrepareStoredServers(run.AppID, run.ID, []mcp.RunServerRequest{{
		ServerID: "customer-io-1", ServerName: "customer_io",
		Transport: agentcore.MCPTransportStreamableHTTP, URL: remote.URL,
		Tools:      []mcp.RunTool{{Name: "cio_read_api", Access: agentcore.MCPToolAccessRead}},
		Credential: &mcp.RunCredential{Type: mcp.CredentialBearerToken, AccessToken: "expired-remotely"},
	}}, mcp.RunConfig{CredentialKey: key, AllowHTTP: true, AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.CreateRunWithMCP(ctx, run, servers); err != nil {
		t.Fatal(err)
	}
	eng := New(Config{
		Store: mem, Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider(),
		Runtimes: runtime.NewRegistry(runtime.NewNativeAdapter()),
		RunMCP:   mcp.RunConfig{CredentialKey: key, AllowHTTP: true, AllowPrivateNetwork: true},
	})
	result, err := eng.ExecuteRunOnce(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatalf("authentication should pause instead of fail: %v", err)
	}
	if result == nil || !result.AwaitingAuth {
		t.Fatalf("expected AwaitingAuth result, got %#v", result)
	}
	stored, _ := mem.GetRun(ctx, run.AppID, run.ID)
	if stored.Status != agentcore.RunStatusPaused || stored.PauseReason != agentcore.PauseReasonAuth {
		t.Fatalf("run was not paused for authentication: %#v", stored)
	}
	interactions, _ := mem.ListInteractions(ctx, run.AppID, run.ID)
	if len(interactions) != 1 || interactions[0].InteractionKind != "authentication" ||
		!strings.Contains(string(interactions[0].RequestPayload), "customer-io-1") {
		t.Fatalf("missing structured MCP authentication interaction: %#v", interactions)
	}
	storedServers, _ := mem.ListRunMCPServers(ctx, run.AppID, run.ID)
	if len(storedServers) != 1 || len(storedServers[0].EncryptedCredential) == 0 {
		t.Fatal("paused authentication run did not retain encrypted credential")
	}
}

func TestExecuteRunOncePausesForGatewayCreatedApprovalInteraction(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := &agentcore.Agent{ID: "agent-approval", AppID: "app-a", Name: "Agent", RuntimeKind: "interaction-test", ApprovalMode: agentcore.ApprovalModeMutatingTools}
	if err := mem.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{
		ID: "run-approval", AppID: "app-a", AgentID: agent.ID, RuntimeKind: agent.RuntimeKind,
		Target: agentcore.TargetRef{Type: "task", ID: "task-1"}, Status: agentcore.RunStatusQueued, ApprovalState: agentcore.ApprovalNotRequired,
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	eng := New(Config{
		Store: mem, Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider(),
		Runtimes: runtime.NewRegistry(interactionOnlyAdapter{}),
	})
	result, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.WaitForApproval {
		t.Fatalf("pending gateway interaction did not set wait state: %#v", result)
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil || stored.Status != agentcore.RunStatusPaused || stored.PauseReason != agentcore.PauseReasonHumanApproval {
		t.Fatalf("run was not paused for approval: %#v err=%v", stored, err)
	}
}

type interactionOnlyAdapter struct{}

func (interactionOnlyAdapter) Kind() string { return "interaction-test" }
func (interactionOnlyAdapter) Execute(execCtx *runtime.ExecutionContext) (*runtime.Result, error) {
	if err := execCtx.InteractionBroker.RequestInteraction(execCtx.Context, agentcore.AgentRunInteraction{
		InteractionKind: "approval_request", Status: "pending", Title: "Approve MCP tool",
	}); err != nil {
		return nil, err
	}
	return &runtime.Result{AssistantMessage: "Approval is required."}, nil
}

type recordingRuntimeAdapter struct {
	kind               string
	lease              *agentcore.WorkspaceLease
	calls              int
	skillRefs          []agentcore.SkillRef
	availableSkillRefs []agentcore.SkillRef
	skillInstructions  string
	skillPolicy        skills.Policy
	usesSplitSkills    bool
	stagedSkillRoot    string
	outputSummary      json.RawMessage
	toolCalls          []agentcore.ToolCall
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
	a.availableSkillRefs = append([]agentcore.SkillRef(nil), execCtx.AvailableSkillRefs...)
	a.skillInstructions = execCtx.SkillInstructions
	a.skillPolicy = execCtx.SkillPolicy
	a.usesSplitSkills = execCtx.UsesSplitSkills
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

type interruptingRuntimeAdapter struct {
	cancel context.CancelFunc
}

func (a *interruptingRuntimeAdapter) Kind() string {
	return agentcore.RuntimeNativeSDK
}

func (a *interruptingRuntimeAdapter) Execute(_ *runtime.ExecutionContext) (*runtime.Result, error) {
	a.cancel()
	return nil, context.Canceled
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

type recordingRunCloser struct {
	mu    sync.Mutex
	calls int
}

func (c *recordingRunCloser) CloseRun(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil
}

func (c *recordingRunCloser) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
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
