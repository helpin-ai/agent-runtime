package runtime

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// An approved commit_and_push that completes during approval reconciliation
// must not leave its started marker behind: a later cancel would otherwise
// report the persisted, successful push as started_outcome_unknown.
func TestNativeApprovedExternalCallClearsStartedMarker(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	var execCount int32
	registerMutatingCounter(registry, "commit_and_push", &execCount)
	summary, interactionID := pauseForApproval(t, mem, registry, "run-push", "commit_and_push")
	resolveInteraction(t, mem, "app-a", "run-push", interactionID, "approve", "")

	run := &agentcore.AgentRun{
		ID: "run-push", AppID: "app-a", RuntimeKind: agentcore.RuntimeNativeSDK,
		Target:        agentcore.TargetRef{Type: "workspace", ID: "W-1"},
		Input:         agentcore.RunInput{Instructions: "write a report", Metadata: map[string]any{"last_resume": map[string]any{"intent": "approve"}}},
		OutputSummary: summary,
	}
	execCtx := &ExecutionContext{
		Context: context.Background(), AppID: "app-a", Store: mem,
		Agent: &agentcore.Agent{Name: "Native", RuntimeKind: agentcore.RuntimeNativeSDK, ApprovalMode: agentcore.ApprovalModeAlways, AllowedTools: []string{"commit_and_push"}},
		Run:   run, Tools: registry, AllowedTools: map[string]bool{"commit_and_push": true},
	}
	recorder, err := openNativeRecorder(execCtx.Context, execCtx, false)
	if err != nil {
		t.Fatal(err)
	}
	messages, _, err := recorder.initialMessages(false)
	if err != nil {
		t.Fatal(err)
	}
	result := &nativeExecutionResult{Messages: append([]NativeMessage(nil), messages...)}
	if err := recorder.save(execCtx.Context, "approval_tools", result); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(execCtx.Context, nativeCallRecorderKey{}, &nativeCallRecorder{recorder: recorder, result: result})

	reconciled, err := nativeReconcileResumedApprovals(ctx, execCtx, messages)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if atomic.LoadInt32(&execCount) != 1 {
		t.Fatalf("approved push must execute exactly once, count=%d", execCount)
	}
	if len(recorder.state.StartedCalls) != 0 {
		t.Fatalf("started marker retained after persisted result: %v", recorder.state.StartedCalls)
	}
	unknown, err := NativeInterruptedEffects(ctx, mem, "app-a", "run-push")
	if err != nil {
		t.Fatal(err)
	}
	if unknown != nil {
		t.Fatalf("completed approved push reported as interrupted: %s", unknown)
	}
	// The checkpoint carries the real result, not the approval placeholder.
	var sawReal bool
	for _, m := range reconciled {
		for _, b := range m.Blocks {
			if b.Type == nativeBlockTypeToolResult && b.ToolCallID == "tc-1" {
				var placeholder nativeApprovalBlockState
				_ = json.Unmarshal([]byte(b.Output), &placeholder)
				sawReal = !placeholder.ApprovalRequired
			}
		}
	}
	if !sawReal {
		t.Fatal("reconciled transcript still holds the approval placeholder")
	}
}
