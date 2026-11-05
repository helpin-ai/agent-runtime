package workjobs

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// inlineDurable stands in for Temporal: it executes durable runs through
// the engine, as the durable activity does.
type inlineDurable struct {
	engine  atomic.Pointer[engine.Engine]
	started atomic.Int32
}

func (d *inlineDurable) StartRun(_ context.Context, run *agentcore.AgentRun) error {
	d.started.Add(1)
	go func() { _, _ = d.engine.Load().ExecuteRunOnce(context.Background(), run.AppID, run.ID) }()
	return nil
}
func (d *inlineDurable) CancelRun(context.Context, *agentcore.AgentRun) error { return nil }
func (d *inlineDurable) ResumeRun(context.Context, *agentcore.AgentRun, engine.ResumePayload) error {
	return nil
}

func openPostgresJobs(t *testing.T, dsn string) (*store.SQL, string) {
	t.Helper()
	admin, err := store.OpenSQL(store.SQLConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("work_jobs_deployed_%d", time.Now().UnixNano())
	if err := admin.DB().Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	scoped := dsn + separator + "search_path=" + schema
	t.Cleanup(func() {
		admin.DB().Exec("DROP SCHEMA " + schema + " CASCADE")
		if conn, err := admin.DB().DB(); err == nil {
			conn.Close()
		}
	})
	s, err := store.OpenSQL(store.SQLConfig{Driver: "postgres", DSN: scoped})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MigratePostgres(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.MigratePostgresJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, scoped
}

// Phase 1b: on PostgreSQL with durable workers and two controller replicas,
// one leads, the job completes and every item is accepted exactly once.
func TestDeployedJobWithDurableWorkersAndTwoReplicas(t *testing.T) {
	dsn := os.Getenv("AGENT_RUNTIME_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set AGENT_RUNTIME_TEST_POSTGRES_DSN to test deployed work jobs")
	}
	model := &scriptedWorker{script: submitAll}
	durable := &inlineDurable{}
	var leaders atomic.Int32
	var services []*Service
	var cancel []context.CancelFunc
	var wg sync.WaitGroup
	var first *store.SQL
	for replica := 0; replica < 2; replica++ {
		var s *store.SQL
		if replica == 0 {
			s, _ = openPostgresJobs(t, dsn)
			first = s
		} else {
			s = store.NewSQL(first.DB())
			if err := s.MigratePostgresJobs(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		targets := host.NewStaticContextProvider()
		targets.Register(testApp, testTarget, host.TargetContext{Summary: "batch"})
		registry := tools.NewRegistry()
		eng := engine.New(engine.Config{
			DefaultExecutionMode: engine.ExecutionModeDurable, Durable: durable, Store: s, Tools: registry, Targets: targets,
			Runtimes: runtime.NewRegistry(runtime.NewNativeAdapterWithConfig(runtime.NativeConfig{ModelFactory: model, MaxToolSteps: 40})),
		})
		if replica == 0 {
			durable.engine.Store(eng)
		}
		leader := store.NewWorkJobLeader(s)
		t.Cleanup(leader.Release)
		var led atomic.Bool
		service := New(Config{Engine: eng, Store: s, Tools: registry, Interval: 20 * time.Millisecond,
			Enabled: func(appID string) bool { return appID == testApp },
			Leader: func(ctx context.Context) bool {
				ok := leader.Lead(ctx)
				if ok && led.CompareAndSwap(false, true) {
					leaders.Add(1)
				}
				return ok
			}})
		ctx, stop := context.WithCancel(context.Background())
		cancel = append(cancel, stop)
		services = append(services, service)
		wg.Add(1)
		go func() { defer wg.Done(); service.Run(ctx) }()
	}
	t.Cleanup(func() {
		for _, stop := range cancel {
			stop()
		}
		wg.Wait()
	})
	agent := agentcore.Agent{AppID: testApp, ID: "researcher", Name: "Researcher", RuntimeKind: agentcore.RuntimeNativeSDK}
	if err := first.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.MaxConcurrency = 2
	job, err := services[1].Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp, Spec: spec,
		Worker: agentcore.WorkJobWorker{AgentID: "researcher", Target: testTarget}, Items: items(200)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		current, err := first.GetWorkJob(context.Background(), testApp, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == agentcore.WorkJobStateCompleted {
			job = current
			break
		}
		if time.Now().After(deadline) || agentcore.IsTerminalWorkJobState(current.State) || current.State == agentcore.WorkJobStateWaiting {
			t.Fatalf("job = %s/%s %+v %s", current.State, current.WaitingReason, current.Counts, current.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job.Counts.Found != 200 || durable.started.Load() != 20 {
		t.Fatalf("counts = %+v, durable workers started = %d", job.Counts, durable.started.Load())
	}
	if leaders.Load() != 1 {
		t.Fatalf("%d replicas led", leaders.Load())
	}
	accepted := map[string]int{}
	for after := int64(0); ; {
		page, err := first.ListWorkJobEvents(context.Background(), testApp, job.ID, after, 500)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Items {
			if event.Type == "work_item.accepted" {
				accepted[event.Data["item_id"].(string)]++
			}
		}
		if len(page.Items) < 500 {
			break
		}
		after = page.NextAfter
	}
	for id, n := range accepted {
		if n != 1 {
			t.Fatalf("%s accepted %d times", id, n)
		}
	}
	if len(accepted) != 200 {
		t.Fatalf("accepted %d items", len(accepted))
	}
}
