package engine

import (
	"context"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type remoteCancelAdapter struct {
	calls  int
	target *host.TargetContext
	status string
}

func (a *remoteCancelAdapter) Kind() string { return agentcore.RuntimeA2A }

func (a *remoteCancelAdapter) Execute(*runtime.ExecutionContext) (*runtime.Result, error) {
	return &runtime.Result{}, nil
}

func (a *remoteCancelAdapter) NeedsRemoteCancel(run *agentcore.AgentRun) bool {
	_, ok := run.Input.Metadata["a2a"]
	return ok
}

func (a *remoteCancelAdapter) CancelRemote(_ context.Context, run *agentcore.AgentRun, target *host.TargetContext) error {
	a.calls++
	a.target = target
	a.status = run.Status
	return nil
}

func TestCancelRunCancelsRemoteWorkWhileRunIsActive(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID: "r", AppID: "a", AgentID: "agent", RuntimeKind: agentcore.RuntimeA2A, Status: agentcore.RunStatusPaused,
		PauseReason: agentcore.PauseReasonHumanInput, Target: agentcore.TargetRef{Type: "task", ID: "T-1"},
		Input: agentcore.RunInput{Metadata: map[string]interface{}{"a2a": map[string]interface{}{"task_id": "remote-1", "state": "input_required"}}},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	adapter := &remoteCancelAdapter{}
	eng := New(Config{Store: mem, Runtimes: runtime.NewRegistry(adapter), Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider()})
	cancelled, err := eng.CancelRun(ctx, "a", "r")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != agentcore.RunStatusCancelled {
		t.Fatalf("run was not cancelled: %s", cancelled.Status)
	}
	if adapter.calls != 1 || adapter.target == nil || adapter.target.Target.ID != "T-1" {
		t.Fatalf("remote cancel was not called with a resolved target: calls=%d target=%+v", adapter.calls, adapter.target)
	}
	// The host only returns connection details for active runs.
	if adapter.status != agentcore.RunStatusPaused {
		t.Fatalf("remote cancel ran after the run was terminal: %s", adapter.status)
	}

	nativeRun := &agentcore.AgentRun{ID: "n", AppID: "a", AgentID: "agent", RuntimeKind: agentcore.RuntimeNativeSDK, Status: agentcore.RunStatusPaused, Target: agentcore.TargetRef{Type: "task", ID: "T-2"}}
	if err := mem.CreateRun(ctx, nativeRun); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.CancelRun(ctx, "a", "n"); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 {
		t.Fatal("remote cancel ran for a runtime without remote work")
	}
}
