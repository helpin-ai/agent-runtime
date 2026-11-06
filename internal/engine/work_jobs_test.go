package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// overlapAdapter records how many executions of one run overlap and which
// tools each execution was offered.
type overlapAdapter struct {
	mu       sync.Mutex
	active   int
	maxSeen  int
	calls    int
	gate     chan struct{}
	offered  []map[string]bool
	firstRun chan struct{}
}

func (a *overlapAdapter) Kind() string { return agentcore.RuntimeNativeSDK }

func (a *overlapAdapter) Execute(execCtx *runtime.ExecutionContext) (*runtime.Result, error) {
	a.mu.Lock()
	a.active++
	a.calls++
	call := a.calls
	a.maxSeen = max(a.maxSeen, a.active)
	offered := map[string]bool{}
	for name, ok := range execCtx.AllowedTools {
		offered[name] = ok
	}
	a.offered = append(a.offered, offered)
	a.mu.Unlock()
	if call == 1 && a.gate != nil {
		close(a.firstRun)
		<-a.gate
	}
	a.mu.Lock()
	a.active--
	a.mu.Unlock()
	return &runtime.Result{AssistantMessage: "done", OutputSummary: json.RawMessage(`{}`)}, nil
}

func adapterEngine(mem *store.Memory, adapter runtime.Adapter) *Engine {
	targets := host.NewStaticContextProvider()
	targets.Register("app-a", agentcore.TargetRef{Type: "ticket", ID: "T-1"}, host.TargetContext{Summary: "ticket"})
	return New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              targets,
	})
}

// A resume that races the previous turn's wind-down must neither run
// concurrently with it nor be dropped: launches coalesce into one more
// execution after the current one returns.
func TestLaunchGuardSerializesAndKeepsRacingResume(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	adapter := &overlapAdapter{gate: make(chan struct{}), firstRun: make(chan struct{})}
	eng := adapterEngine(mem, adapter)
	run, err := eng.StartRun(ctx, StartRunRequest{AppID: "app-a", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		TurnPolicy: agentcore.TurnPolicy{Mode: agentcore.TurnPolicyPauseAfterAssist}})
	if err != nil {
		t.Fatal(err)
	}
	<-adapter.firstRun
	if !eng.IsExecuting("app-a", run.ID) {
		t.Fatal("run not registered as executing")
	}
	eng.launchLightweight("app-a", run.ID)
	eng.launchLightweight("app-a", run.ID)
	if recovered, _ := eng.RecoverWorkJobRun(ctx, "app-a", run.ID); recovered {
		t.Fatal("recovery started a run that is executing")
	}
	close(adapter.gate)
	deadline := time.Now().Add(3 * time.Second)
	for eng.IsExecuting("app-a", run.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.maxSeen != 1 || adapter.calls != 2 {
		t.Fatalf("max concurrent = %d, executions = %d; want 1 and 2", adapter.maxSeen, adapter.calls)
	}
}

func TestSubmitToolOfferedOnlyToWorkJobWorkers(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	// Even an agent that lists the reserved tool gets it only as a worker.
	agent.AllowedTools = append(agent.AllowedTools, WorkJobSubmitTool)
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	jobGroup, err := mem.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a", OwnerKind: agentcore.RunGroupOwnerWorkJob})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &overlapAdapter{}
	eng := adapterEngine(mem, adapter)
	target := agentcore.TargetRef{Type: "ticket", ID: "T-1"}
	if _, err := eng.StartRun(ctx, StartRunRequest{AppID: "app-a", AgentID: agent.ID, Target: target, GroupID: jobGroup.ID}); !errors.Is(err, ErrRunGroupsDisabled) && !errors.Is(err, agentcore.ErrRunGroupNotFound) {
		t.Fatalf("host joined a work-job group: %v", err)
	}
	ordinary, err := eng.StartRun(ctx, StartRunRequest{AppID: "app-a", AgentID: agent.ID, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, mem, "app-a", ordinary.ID, agentcore.RunStatusCompleted)
	worker, err := eng.StartWorkJobRun(ctx, StartRunRequest{AppID: "app-a", AgentID: agent.ID, Target: target, GroupID: jobGroup.ID, HostRunID: "work-job:att-1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, mem, "app-a", worker.ID, agentcore.RunStatusCompleted)
	again, err := eng.StartWorkJobRun(ctx, StartRunRequest{AppID: "app-a", AgentID: agent.ID, Target: target, GroupID: jobGroup.ID, HostRunID: "work-job:att-1"}, nil)
	if err != nil || again.ID != worker.ID {
		t.Fatalf("dispatch key replay created another run: %v %v", again, err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.offered) != 2 || adapter.offered[0][WorkJobSubmitTool] || !adapter.offered[1][WorkJobSubmitTool] {
		t.Fatalf("offered tools = %v", adapter.offered)
	}
}
