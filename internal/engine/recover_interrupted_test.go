package engine

import (
	"context"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestRecoverInterruptedLightweightRunsOnlyRedispatchesLightweightWork(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	runs := []*agentcore.AgentRun{
		// The agent is missing, so re-execution fails the run: proof it was dispatched.
		{ID: "run-interrupted", AppID: "app-a", AgentID: "missing", ExecutionMode: ExecutionModeLightweight, Status: agentcore.RunStatusRunning},
		{ID: "run-durable", AppID: "app-a", AgentID: "missing", ExecutionMode: ExecutionModeDurable, Status: agentcore.RunStatusRunning},
		{ID: "run-paused", AppID: "app-a", AgentID: "missing", ExecutionMode: ExecutionModeLightweight, Status: agentcore.RunStatusPaused, PauseReason: agentcore.PauseReasonUserMessage},
	}
	for _, run := range runs {
		if err := mem.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	eng := New(Config{Store: mem})
	count, err := eng.RecoverInterruptedLightweightRuns(ctx)
	if err != nil || count != 1 {
		t.Fatalf("want one recovered run, got %d, %v", count, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, _ := mem.GetRun(ctx, "app-a", "run-interrupted")
		if stored.Status == agentcore.RunStatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("interrupted run was not re-executed: %s", stored.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for id, want := range map[string]string{"run-durable": agentcore.RunStatusRunning, "run-paused": agentcore.RunStatusPaused} {
		stored, _ := mem.GetRun(ctx, "app-a", id)
		if stored.Status != want {
			t.Fatalf("%s must be left alone, got %s", id, stored.Status)
		}
	}
}

func TestRecoverInterruptedLightweightRunsRespectsManualExecution(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{ID: "run-cli", AppID: "app-a", AgentID: "missing", ExecutionMode: ExecutionModeLightweight, Status: agentcore.RunStatusRunning}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	count, err := New(Config{Store: mem, ManualLightweightExecution: true}).RecoverInterruptedLightweightRuns(ctx)
	if err != nil || count != 0 {
		t.Fatalf("manual execution owns its runs, got %d, %v", count, err)
	}
}
