package workjobs

import (
	"context"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"sync"
	"testing"
	"time"
)

func TestSteeringReachesAll851ItemsWithoutReplacingJob(t *testing.T) {
	h := newHarness(t, tempDB(t), &scriptedWorker{script: submitAll})
	spec := testSpec()
	spec.BatchSize = 5
	spec.MaxConcurrency = 3
	spec.Steering = &sdk.WorkJobSteering{Content: "Research only; retain verified evidence", DeniedTools: []string{"crm_write"}}
	job := h.create(spec, 851)
	done := h.waitState(job.ID, 2*time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateFailed, agentcore.WorkJobStateWaiting)
	if done.State != agentcore.WorkJobStateCompleted || done.Counts.Found != 851 {
		t.Fatalf("job: %+v", done)
	}
	for _, attempt := range h.attempts(job.ID) {
		commands, err := h.engine.RunSteering(context.Background(), testApp, attempt.RunID)
		if err != nil || len(commands) != 1 || commands[0].State != "applied" {
			t.Fatalf("attempt %s: %+v %v", attempt.ID, commands, err)
		}
	}
}

func TestInstructionRevisionKeepsBatchAndAcceptedResults(t *testing.T) {
	ctx := context.Background()
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	model := &scriptedWorker{script: func(batch, accepted []string, step int) ([]string, bool) {
		if step == 0 {
			return batch[:3], false
		}
		remaining := unresolved(batch, accepted)
		return remaining, len(remaining) == 0
	}, beforeGo: func(_ []string, step int) {
		if step == 3 {
			once.Do(func() { close(reached); <-release })
		}
	}}
	h := newHarness(t, tempDB(t), model)
	job := h.create(testSpec(), 10)
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("worker never reached partial batch")
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	spec := job.Spec
	spec.Steering = &sdk.WorkJobSteering{Content: "Keep the evidence in the revised deliverable"}
	revised, err := h.service.Revise(ctx, testApp, job.ID, agentcore.ReviseWorkJobRequest{ExpectedRevision: job.SpecRevision, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		current, err := h.service.Get(ctx, testApp, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(current.ActiveRuns) > 0 {
			commands, err := h.engine.RunSteering(ctx, testApp, current.ActiveRuns[0])
			if err != nil {
				t.Fatal(err)
			}
			if len(commands) > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("revision never delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	done := h.waitState(job.ID, 10*time.Second, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateFailed, agentcore.WorkJobStateWaiting)
	if done.Counts.Found != 10 || done.State != agentcore.WorkJobStateCompleted || len(h.attempts(job.ID)) != 1 {
		t.Fatalf("revision replaced batch: %+v attempts=%d", done, len(h.attempts(job.ID)))
	}
	dataset, err := h.service.Dataset(ctx, testApp, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range dataset.Items {
		want := revised.SpecRevision
		if i < 3 {
			want = job.SpecRevision
		}
		if item.SpecRevision != want {
			t.Fatalf("item %s revision=%d want=%d", item.ID, item.SpecRevision, want)
		}
	}
}
