package workjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Phase 1a gate: a 1,000-item job completes in bounded batches of ten with
// no prompt from anyone, and every input is accounted for in the dataset.
func TestThousandItemJobCompletesInBoundedBatches(t *testing.T) {
	h := newHarness(t, tempDB(t), &scriptedWorker{script: submitAll})
	job := h.create(testSpec(), 1000)
	job = h.waitState(job.ID, 2*time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateFailed, agentcore.WorkJobStateWaiting)
	if job.State != agentcore.WorkJobStateCompleted {
		t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
	}
	if job.Counts.Found != 1000 || job.Counts.Total != 1000 {
		t.Fatalf("counts = %+v", job.Counts)
	}
	attempts := h.attempts(job.ID)
	if len(attempts) != 100 {
		t.Fatalf("clean 1,000-item job used %d attempts, want 100", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.State != agentcore.WorkAttemptStateCompleted || len(attempt.ItemIDs) != 10 || attempt.Accepted != 10 {
			t.Fatalf("attempt = %+v", attempt)
		}
	}
	dataset, err := h.service.Dataset(context.Background(), testApp, job.ID)
	if err != nil || len(dataset.Items) != 1000 {
		t.Fatalf("dataset = %d items, %v", len(dataset.Items), err)
	}
	for i, item := range dataset.Items {
		want := fmt.Sprintf("item-%04d", i+1)
		if item.ID != want || string(item.Result) != fmt.Sprintf(`{"value":%q}`, want) {
			t.Fatalf("dataset item %d = %+v", i, item)
		}
	}
	// Two model calls per attempt at 110 tokens each, applied once.
	if job.Usage.InputTokens != 100*2*100 || job.Usage.OutputTokens != 100*2*10 || job.Usage.ReservedTokens != 0 {
		t.Fatalf("usage = %+v", job.Usage)
	}
	runs, err := h.store.ListRuns(context.Background(), testApp)
	if err != nil || len(runs) != 0 {
		t.Fatalf("job workers listed by default: %d runs, %v", len(runs), err)
	}
	page, err := h.store.SearchRuns(context.Background(), agentcore.RunSearch{AppID: testApp})
	if err != nil || page.Total != 0 {
		t.Fatalf("job workers counted by default search: %+v, %v", page, err)
	}
	all, err := h.store.SearchRuns(context.Background(), agentcore.RunSearch{AppID: testApp, IncludeHiddenGroupMembers: true, Limit: 200})
	if err != nil || all.Total != 100 {
		t.Fatalf("explicit worker listing = %d, %v", all.Total, err)
	}
	for _, run := range all.Items {
		interactions, err := h.store.ListInteractions(context.Background(), testApp, run.ID)
		if err != nil || len(interactions) != 0 {
			t.Fatalf("worker %s asked for input: %d, %v", run.ID, len(interactions), err)
		}
	}
	t.Logf("1,000 items: %d attempts, %d model calls, %d job events", len(attempts), h.model.calls.Load(), lastEventSeq(t, h, job.ID))
}

func lastEventSeq(t *testing.T, h *harness, jobID string) int64 {
	page, err := h.store.ListWorkJobEvents(context.Background(), testApp, jobID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	last := page.NextAfter
	for len(page.Items) == 500 {
		if page, err = h.store.ListWorkJobEvents(context.Background(), testApp, jobID, last, 500); err != nil {
			t.Fatal(err)
		}
		last = page.NextAfter
	}
	return last
}

// A worker that claims it is done after three of ten items does not
// complete its batch: the attempt is partial, accepted items stay accepted
// and only the unresolved items go to a fresh worker.
func TestWorkerStoppingEarlyIsPartialAndOnlyUnresolvedRetry(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	stoppers := map[string]bool{}
	stopped := 0
	model := &scriptedWorker{script: func(batch, accepted []string, step int) ([]string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if step == 0 {
			fresh := true
			for _, id := range batch {
				fresh = fresh && !seen[id]
				seen[id] = true
			}
			if fresh {
				// A fresh batch: submit three items, then claim to be done.
				stopped++
				stoppers[batch[0]] = true
				return batch[:min(3, len(batch))], false
			}
		}
		remaining := unresolved(batch, accepted)
		if stoppers[batch[0]] && step > 0 {
			return nil, true
		}
		return remaining, len(remaining) == 0
	}}
	h := newHarness(t, tempDB(t), model)
	job := h.create(testSpec(), 30)
	job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateFailed)
	if job.State != agentcore.WorkJobStateCompleted || job.Counts.Found != 30 {
		t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
	}
	partial, accepted := 0, 0
	for _, attempt := range h.attempts(job.ID) {
		accepted += attempt.Accepted
		if attempt.State == agentcore.WorkAttemptStatePartial {
			partial++
			if attempt.Accepted != min(3, len(attempt.ItemIDs)) {
				t.Fatalf("partial attempt = %+v", attempt)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if partial == 0 || partial != stopped || accepted != 30 {
		t.Fatalf("early stops = %d, partial attempts = %d, accepted results = %d; each item must be accepted exactly once", stopped, partial, accepted)
	}
}

// Restart: a worker is interrupted mid-batch. A new process recovers it from
// its checkpoint under a new execution generation; accepted items are not
// redone, and a late submission from the old execution is fenced off.
func TestRestartRecoversWorkerAndFencesOldExecution(t *testing.T) {
	path := tempDB(t)
	release := make(chan struct{})
	interrupted := make(chan struct{})
	var once sync.Once
	stale := &scriptedWorker{script: func(batch, accepted []string, step int) ([]string, bool) {
		if step == 0 {
			return batch[:3], false
		}
		// Simulated crash: this execution stops making progress.
		once.Do(func() { close(interrupted) })
		<-release
		return unresolved(batch, accepted)[:1], false
	}}
	first := newHarness(t, path, stale)
	job := first.create(testSpec(), 10)
	select {
	case <-interrupted:
	case <-time.After(30 * time.Second):
		t.Fatal("worker never reached the interruption point")
	}
	first.stop()

	recovered := &scriptedWorker{script: submitAll}
	second := newHarness(t, path, recovered)
	job = second.waitState(job.ID, time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateFailed)
	if job.State != agentcore.WorkJobStateCompleted || job.Counts.Found != 10 {
		t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
	}
	attempts := second.attempts(job.ID)
	if len(attempts) != 1 || attempts[0].Generation != 1 {
		t.Fatalf("recovery must continue the same attempt under generation 1: %+v", attempts)
	}
	if len(recovered.batches) != 0 {
		t.Fatalf("recovery restarted the batch instead of continuing it: %v", recovered.batches)
	}
	// The old execution wakes up and submits: it is fenced.
	close(release)
	time.Sleep(300 * time.Millisecond)
	dataset, err := second.service.Dataset(context.Background(), testApp, job.ID)
	if err != nil || len(dataset.Items) != 10 {
		t.Fatalf("dataset = %v, %v", dataset, err)
	}
	events, _ := second.store.ListWorkJobEvents(context.Background(), testApp, job.ID, 0, 500)
	accepted := 0
	for _, event := range events.Items {
		if event.Type == "work_item.accepted" {
			accepted++
		}
	}
	if accepted != 10 {
		t.Fatalf("items accepted %d times, want exactly 10", accepted)
	}
}

// Validation errors go back to the worker as actionable field errors; the
// corrected resubmission is accepted.
func TestInvalidResultIsRejectedWithFieldErrors(t *testing.T) {
	var mu sync.Mutex
	bad := map[string]bool{}
	model := &scriptedWorker{script: submitAll}
	model.outcome = func(itemID string) (string, json.RawMessage, *agentcore.WorkItemException) {
		mu.Lock()
		defer mu.Unlock()
		if itemID == "item-0002" && !bad[itemID] {
			bad[itemID] = true
			return agentcore.WorkItemOutcomeCompleted, json.RawMessage(`{"value":""}`), nil
		}
		if itemID == "item-0003" && !bad["unreachable"] {
			bad["unreachable"] = true
			// An unreachable source reported as no_result is refused.
			return agentcore.WorkItemOutcomeNoResult, nil, &agentcore.WorkItemException{Kind: "site_unreachable", Detail: "timeout"}
		}
		return agentcore.WorkItemOutcomeCompleted, json.RawMessage(fmt.Sprintf(`{"value":%q}`, itemID)), nil
	}
	h := newHarness(t, tempDB(t), model)
	job := h.create(testSpec(), 5)
	job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateFailed)
	if job.State != agentcore.WorkJobStateCompleted || job.Counts.Found != 5 {
		t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
	}
	attempts := h.attempts(job.ID)
	calls, err := h.store.ListToolCalls(context.Background(), testApp, attempts[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	var errorsSeen []string
	for _, call := range calls {
		if call.ToolName == submitToolName && call.Error != "" {
			errorsSeen = append(errorsSeen, call.Error)
		}
	}
	joined := strings.Join(errorsSeen, "\n")
	if len(errorsSeen) != 2 || !containsAll(joined, `"item-0002"`, "result", `"item-0003"`, "not no_result") {
		t.Fatalf("rejections = %q", errorsSeen)
	}
}

// An unreachable site is retried in fresh workers, then accepted as an
// exception, never as no_result. Exceptions above the policy make the job
// wait for the user; accepting them completes it.
func TestRetryableExceptionsAndExceptionPolicy(t *testing.T) {
	model := &scriptedWorker{script: submitAll}
	model.outcome = func(itemID string) (string, json.RawMessage, *agentcore.WorkItemException) {
		if itemID == "item-0001" || itemID == "item-0002" {
			return agentcore.WorkItemOutcomeException, nil, &agentcore.WorkItemException{Kind: "site_unreachable", Detail: "connection timed out"}
		}
		if itemID == "item-0003" {
			return agentcore.WorkItemOutcomeException, nil, &agentcore.WorkItemException{Kind: "needs_login", Detail: "members only"}
		}
		return agentcore.WorkItemOutcomeCompleted, json.RawMessage(fmt.Sprintf(`{"value":%q}`, itemID)), nil
	}
	h := newHarness(t, tempDB(t), model)
	spec := testSpec()
	spec.ExceptionKinds = []string{"site_unreachable", "needs_login"}
	spec.RetryableExceptionKinds = []string{"site_unreachable"}
	job := h.create(spec, 10)
	job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateCompleted)
	if job.State != agentcore.WorkJobStateWaiting || job.WaitingReason != agentcore.WorkJobWaitingExceptions {
		t.Fatalf("3 exceptions of 10 exceed the 10%% policy; job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
	}
	if job.Counts.Exceptions != 3 || job.Counts.NoResult != 0 || job.Counts.Found != 7 {
		page, _ := h.store.ListWorkItems(context.Background(), testApp, job.ID, "", "", 3)
		for _, item := range page.Items {
			t.Logf("%s state=%s attempts=%d outcome=%s err=%q", item.ID, item.State, item.Attempts, item.Outcome, item.LastError)
		}
		for _, attempt := range h.attempts(job.ID) {
			t.Logf("attempt %s state=%s items=%v accepted=%d outcome=%q", attempt.ID, attempt.State, attempt.ItemIDs, attempt.Accepted, attempt.Outcome)
		}
		t.Fatalf("counts = %+v", job.Counts)
	}
	items, _ := h.store.ListWorkItems(context.Background(), testApp, job.ID, "", "", 3)
	if items.Items[0].Attempts != 3 || items.Items[2].Attempts != 1 {
		t.Fatalf("retryable exception attempts = %d, non-retryable = %d", items.Items[0].Attempts, items.Items[2].Attempts)
	}
	dataset, _ := h.service.Dataset(context.Background(), testApp, job.ID)
	if len(dataset.Items) != 10 {
		t.Fatalf("waiting job must still publish its dataset: %d items", len(dataset.Items))
	}
	if _, err := h.service.Resume(context.Background(), testApp, job.ID); err != nil {
		t.Fatal(err)
	}
	job = h.waitState(job.ID, 30*time.Second, agentcore.WorkJobStateCompleted)
}

func TestNoProgressWaitsForUser(t *testing.T) {
	h := newHarness(t, tempDB(t), &scriptedWorker{script: func([]string, []string, int) ([]string, bool) { return nil, true }})
	job := h.create(testSpec(), 4)
	job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateCompleted)
	if job.WaitingReason != agentcore.WorkJobWaitingNoProgress {
		t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
	}
	if n := len(h.attempts(job.ID)); n != 3 {
		t.Fatalf("attempts before waiting = %d, want 3", n)
	}
}

func TestBudgetStopsAdmission(t *testing.T) {
	h := newHarness(t, tempDB(t), &scriptedWorker{script: submitAll})
	spec := testSpec()
	spec.Budget = agentcore.WorkJobBudget{MaxTokens: 1000, ReserveTokensPerAttempt: 300}
	job := h.create(spec, 50)
	job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateCompleted)
	if job.WaitingReason != agentcore.WorkJobWaitingBudget {
		t.Fatalf("job = %s/%s", job.State, job.WaitingReason)
	}
	used := job.Usage.InputTokens + job.Usage.OutputTokens
	if used+job.Usage.ReservedTokens > 1000 || job.Usage.ReservedTokens != 0 {
		t.Fatalf("budget overrun: %+v", job.Usage)
	}
	// Raising the budget and resuming continues the job.
	spec.Budget.MaxTokens = 100000
	if _, err := h.service.Revise(context.Background(), testApp, job.ID, agentcore.ReviseWorkJobRequest{ExpectedRevision: 1, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Resume(context.Background(), testApp, job.ID); err != nil {
		t.Fatal(err)
	}
	h.waitState(job.ID, time.Minute, agentcore.WorkJobStateCompleted)
}

func TestPauseResumeAndCancelKeepAcceptedResults(t *testing.T) {
	gates := make(chan chan struct{}, 2)
	first, second := make(chan struct{}), make(chan struct{})
	gates <- first
	gates <- second
	model := &scriptedWorker{script: submitAll, beforeGo: func(batch []string, step int) {
		if batch[0] == "item-0011" && step == 0 {
			<-<-gates
		}
	}}
	h := newHarness(t, tempDB(t), model)
	waitSecondBatch := func(jobID string) string {
		deadline := time.Now().Add(30 * time.Second)
		for {
			current, _ := h.store.GetWorkJob(context.Background(), testApp, jobID)
			if current.Counts.Found == 10 && current.Counts.Running == 10 && len(current.ActiveRuns) == 1 {
				return current.ActiveRuns[0]
			}
			if time.Now().After(deadline) {
				t.Fatalf("second batch never started: %+v", current)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	job := h.create(testSpec(), 30)
	waitSecondBatch(job.ID)
	if _, err := h.service.Pause(context.Background(), testApp, job.ID); err != nil {
		t.Fatal(err)
	}
	close(first)
	time.Sleep(500 * time.Millisecond)
	paused, _ := h.store.GetWorkJob(context.Background(), testApp, job.ID)
	if paused.State != agentcore.WorkJobStatePaused || paused.Counts.Found != 20 || len(h.attempts(job.ID)) != 2 {
		t.Fatalf("pause must let the running batch finish and dispatch nothing new: %s %+v", paused.State, paused.Counts)
	}
	if _, err := h.service.Resume(context.Background(), testApp, job.ID); err != nil {
		t.Fatal(err)
	}
	h.waitState(job.ID, 30*time.Second, agentcore.WorkJobStateCompleted)

	// Cancel while a worker runs: the worker is cancelled, accepted results
	// stay in the dataset, unstarted items are cancelled.
	other, err := h.service.Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp, Spec: testSpec(),
		Worker: agentcore.WorkJobWorker{AgentID: h.agent(), Target: testTarget}, Items: items(30)})
	if err != nil {
		t.Fatal(err)
	}
	activeRun := waitSecondBatch(other.ID)
	cancelled, err := h.service.Cancel(context.Background(), testApp, other.ID)
	close(second)
	if err != nil || cancelled.State != agentcore.WorkJobStateCancelled {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
	if cancelled.Counts.Found != 10 || cancelled.Counts.Cancelled != 20 {
		t.Fatalf("cancelled counts = %+v", cancelled.Counts)
	}
	dataset, _ := h.service.Dataset(context.Background(), testApp, other.ID)
	if len(dataset.Items) != 10 {
		t.Fatalf("accepted results lost on cancel: %d", len(dataset.Items))
	}
	run, _ := h.store.GetRun(context.Background(), testApp, activeRun)
	if run == nil || run.Status != agentcore.RunStatusCancelled {
		t.Fatalf("active worker not cancelled: %+v", run)
	}
	time.Sleep(300 * time.Millisecond)
	if after, _ := h.store.GetWorkJob(context.Background(), testApp, other.ID); after.Counts.Found != 10 {
		t.Fatalf("a cancelled worker's late submission was accepted: %+v", after.Counts)
	}
}

// Phase 2: bounded parallel workers produce the same validated coverage as
// sequential processing, with no item accepted twice.
func TestParallelWorkersMatchSequentialCoverage(t *testing.T) {
	var mu sync.Mutex
	active, maxActive := 0, 0
	model := &scriptedWorker{script: submitAll, beforeGo: func(batch []string, step int) {
		if step != 0 {
			return
		}
		mu.Lock()
		active++
		maxActive = max(maxActive, active)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
	}}
	h := newHarness(t, tempDB(t), model)
	spec := testSpec()
	spec.MaxConcurrency = 4
	job := h.create(spec, 200)
	job = h.waitState(job.ID, 2*time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateFailed)
	if job.State != agentcore.WorkJobStateCompleted || job.Counts.Found != 200 {
		t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
	}
	events, _ := h.store.ListWorkJobEvents(context.Background(), testApp, job.ID, 0, 500)
	acceptedCount := map[string]int{}
	for after := int64(0); ; {
		page, _ := h.store.ListWorkJobEvents(context.Background(), testApp, job.ID, after, 500)
		for _, event := range page.Items {
			if event.Type == "work_item.accepted" {
				acceptedCount[event.Data["item_id"].(string)]++
			}
		}
		if len(page.Items) < 500 {
			break
		}
		after = page.NextAfter
	}
	_ = events
	if len(acceptedCount) != 200 {
		t.Fatalf("accepted %d distinct items", len(acceptedCount))
	}
	for id, n := range acceptedCount {
		if n != 1 {
			t.Fatalf("%s accepted %d times", id, n)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive < 2 || maxActive > 4 {
		t.Fatalf("max concurrent workers = %d, want 2..4", maxActive)
	}
}

func TestDependenciesOrderAndPropagateFailure(t *testing.T) {
	model := &scriptedWorker{script: submitAll}
	model.outcome = func(itemID string) (string, json.RawMessage, *agentcore.WorkItemException) {
		if itemID == "b" {
			return agentcore.WorkItemOutcomeException, nil, &agentcore.WorkItemException{Kind: "other", Detail: "cannot decide"}
		}
		return agentcore.WorkItemOutcomeCompleted, json.RawMessage(fmt.Sprintf(`{"value":%q}`, itemID)), nil
	}
	h := newHarness(t, tempDB(t), model)
	spec := testSpec()
	ratio := 1.0
	spec.MaxExceptionRatio = &ratio
	_, err := h.service.Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp, Spec: spec, Worker: agentcore.WorkJobWorker{AgentID: h.agent(), Target: testTarget},
		Items: []agentcore.WorkItemInput{{ID: "x", DependsOn: []string{"y"}}, {ID: "y", DependsOn: []string{"x"}}}})
	var validation *ValidationError
	if !errors.As(err, &validation) || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle = %v", err)
	}
	job, err := h.service.Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp, Spec: spec, Worker: agentcore.WorkJobWorker{AgentID: h.agent(), Target: testTarget},
		Items: []agentcore.WorkItemInput{
			{ID: "aggregate", DependsOn: []string{"a", "c"}},
			{ID: "a"}, {ID: "b"}, {ID: "c", DependsOn: []string{"a"}}, {ID: "after-b", DependsOn: []string{"b"}}, {ID: "after-after-b", DependsOn: []string{"after-b"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateWaiting)
	if job.State != agentcore.WorkJobStateCompleted || job.Counts.Found != 3 || job.Counts.Exceptions != 1 || job.Counts.Failed != 2 {
		t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
	}
	order := map[string]int{}
	for i, batch := range model.batches {
		for _, id := range batch {
			order[id] = i
		}
	}
	if !(order["a"] < order["c"] && order["c"] < order["aggregate"]) {
		t.Fatalf("dependency order violated: %v (batches %v)", order, model.batches)
	}
}

func TestMemoryStoreDoesNotSupportWorkJobs(t *testing.T) {
	service := New(Config{Store: store.NewMemory(), Enabled: func(string) bool { return true }})
	if ok, reason := service.Available(testApp); ok || reason != ReasonUnsupportedStore {
		t.Fatalf("available = %v %s", ok, reason)
	}
	if _, err := service.Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("create on memory store = %v", err)
	}
	disabled := New(Config{Store: openStore(t, tempDB(t))})
	if ok, reason := disabled.Available(testApp); ok || reason != ReasonAppDisabled {
		t.Fatalf("disabled app available = %v %s", ok, reason)
	}
}

// Phase 2 fairness: jobs of one app share its worker cap round-robin, so a
// second job runs alongside a large first one instead of waiting for it.
func TestJobsShareAppWorkerCapFairly(t *testing.T) {
	var mu sync.Mutex
	active := map[string]int{}
	maxTotal := 0
	together := false
	model := &scriptedWorker{script: submitAll, beforeGo: func(batch []string, step int) {
		if step != 0 {
			return
		}
		job := batch[0][:1]
		mu.Lock()
		active[job]++
		total := active["i"] + active["b"]
		maxTotal = max(maxTotal, total)
		together = together || (active["i"] > 0 && active["b"] > 0)
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		mu.Lock()
		active[job]--
		mu.Unlock()
	}}
	h := newHarness(t, tempDB(t), model)
	spec := testSpec()
	spec.MaxConcurrency = 4
	first := h.create(spec, 200)
	second, err := h.service.Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp, Spec: spec,
		Worker: agentcore.WorkJobWorker{AgentID: h.agent(), Target: testTarget}, Items: prefixed("b", 40)})
	if err != nil {
		t.Fatal(err)
	}
	h.waitState(second.ID, time.Minute, agentcore.WorkJobStateCompleted)
	h.waitState(first.ID, 2*time.Minute, agentcore.WorkJobStateCompleted)
	mu.Lock()
	defer mu.Unlock()
	if maxTotal > DefaultMaxWorkersPerApp {
		t.Fatalf("app ran %d workers at once, cap is %d", maxTotal, DefaultMaxWorkersPerApp)
	}
	if !together {
		t.Fatal("the second job waited for the first instead of sharing workers")
	}
}

func prefixed(prefix string, n int) []agentcore.WorkItemInput {
	out := make([]agentcore.WorkItemInput, n)
	for i := range out {
		out[i] = agentcore.WorkItemInput{ID: fmt.Sprintf("%s-%04d", prefix, i+1), Input: json.RawMessage(`{}`)}
	}
	return out
}

// A worker that made changes and then stopped short leaves effects nobody
// can attribute to items: its unresolved items are held, not retried, until
// the user resumes the job.
func TestUnresolvedItemsAfterSideEffectsWaitForUser(t *testing.T) {
	// A write whose response timed out may still have happened remotely.
	for _, timesOut := range []bool{false, true} {
		t.Run(fmt.Sprintf("times_out=%v", timesOut), func(t *testing.T) {
			var sent atomic.Int32
			var mu sync.Mutex
			firstBatch := true
			model := &scriptedWorker{
				script: func(batch, accepted []string, step int) ([]string, bool) {
					mu.Lock()
					defer mu.Unlock()
					if firstBatch {
						if step == 0 {
							return batch[:2], false
						}
						firstBatch = false
						return nil, true
					}
					return submitAll(batch, accepted, step)
				},
				extra: func(batch []string, step int) []runtime.NativeBlock {
					if step != 0 || len(batch) != 5 {
						return nil
					}
					return []runtime.NativeBlock{{Type: "tool_call", ToolCallID: "mail-" + batch[0], ToolName: "send_email", Input: json.RawMessage(`{"to":"x@example.com"}`)}}
				},
			}
			h := newHarness(t, tempDB(t), model)
			h.tools.Register(tools.Definition{Name: "send_email", Description: "Send an email", Mutating: true, RiskLevel: tools.RiskLevelRoutine,
				InputSchema: map[string]any{"type": "object"}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
				sent.Add(1)
				if timesOut {
					// The email went out; only the response was lost.
					return nil, errors.New("send_email: context deadline exceeded")
				}
				return json.RawMessage(`{"sent":true}`), nil
			})
			agent := agentcore.Agent{AppID: testApp, ID: "mailer", Name: "Mailer", RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: []string{"send_email"}, ApprovalMode: "never"}
			if err := h.store.CreateAgent(context.Background(), &agent); err != nil {
				t.Fatal(err)
			}
			job, err := h.service.Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp, Spec: testSpec(),
				Worker: agentcore.WorkJobWorker{AgentID: "mailer", Target: testTarget}, Items: items(5)})
			if err != nil {
				t.Fatal(err)
			}
			job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateCompleted)
			if job.WaitingReason != agentcore.WorkJobWaitingUncertainEffects || job.Counts.Blocked != 3 || job.Counts.Found != 2 {
				t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
			}
			if n := len(h.attempts(job.ID)); n != 1 || sent.Load() != 1 {
				t.Fatalf("held items were retried: attempts=%d emails=%d", n, sent.Load())
			}
			if _, err := h.service.Resume(context.Background(), testApp, job.ID); err != nil {
				t.Fatal(err)
			}
			job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateCompleted)
			if job.Counts.Found != 5 {
				t.Fatalf("counts after resume = %+v", job.Counts)
			}
		})
	}
}

// Phase 3 gate: a parent run delegates discovered work, waits without
// blocking its children, and joins their validated results. Children get
// no delegation tools, so delegation stays one level deep.
// delegatingParent scripts a parent that spawns n subtasks, waits for
// them and stores its last tool output in answer.
func delegatingParent(n int, answer *atomic.Value, follow ...func(lastTool, lastOutput string) *runtime.NativeModelResponse) func(runtime.NativeModelRequest) *runtime.NativeModelResponse {
	return func(req runtime.NativeModelRequest) *runtime.NativeModelResponse {
		var jobID, lastTool, lastOutput string
		for _, message := range req.Messages {
			for _, block := range message.Blocks {
				if block.Type == "tool_result" {
					lastTool, lastOutput = block.ToolName, block.Output
					if block.ToolName == "spawn_subtasks" {
						var out struct {
							JobID string `json:"job_id"`
						}
						_ = json.Unmarshal([]byte(block.Output), &out)
						jobID = out.JobID
					}
				}
			}
		}
		call := func(name string, input any) *runtime.NativeModelResponse {
			raw, _ := json.Marshal(input)
			return &runtime.NativeModelResponse{Message: runtime.NativeMessage{Role: "assistant", Blocks: []runtime.NativeBlock{{Type: "tool_call", ToolCallID: name + "-1", ToolName: name, Input: raw}}}}
		}
		switch {
		case lastTool == "":
			var pages []map[string]any
			for i := 1; i <= n; i++ {
				pages = append(pages, map[string]any{"id": fmt.Sprintf("page-%02d", i), "input": map[string]any{"url": fmt.Sprintf("https://example.com/%d", i)}})
			}
			return call("spawn_subtasks", map[string]any{"objective": "Summarize every page", "instructions": "Summarize one page.",
				"result_schema": json.RawMessage(`{"type":"object","required":["value"]}`), "items": pages, "max_concurrency": 2})
		case lastTool == "spawn_subtasks" || (lastTool == "wait_for_subtasks" && !strings.Contains(lastOutput, `"finished":true`)):
			return call("wait_for_subtasks", map[string]any{"job_id": jobID, "timeout_seconds": 30})
		default:
			for _, next := range follow {
				if response := next(lastTool, lastOutput); response != nil {
					return response
				}
			}
			answer.Store(lastOutput)
			return &runtime.NativeModelResponse{Message: runtime.NativeMessage{Role: "assistant", Content: "All pages summarized."}}
		}
	}
}

func TestParentDelegatesWaitsAndJoinsSubtaskResults(t *testing.T) {
	var answer atomic.Value
	model := &scriptedWorker{script: submitAll}
	model.parent = delegatingParent(25, &answer)
	h := newHarness(t, tempDB(t), model)
	agent := agentcore.Agent{AppID: testApp, ID: "lead", Name: "Lead", RuntimeKind: agentcore.RuntimeNativeSDK, ApprovalMode: "never",
		AllowedTools: []string{"spawn_subtasks", "subtask_status", "wait_for_subtasks", "cancel_subtasks"}}
	if err := h.store.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	parent, err := h.engine.StartRun(context.Background(), engine.StartRunRequest{AppID: testApp, AgentID: "lead", Target: testTarget, Instructions: "Summarize the site."})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		run, _ := h.store.GetRun(context.Background(), testApp, parent.ID)
		if run.Status == agentcore.RunStatusCompleted || run.Status == agentcore.RunStatusFailed {
			if run.Status != agentcore.RunStatusCompleted {
				t.Fatalf("parent run %s: %s", run.Status, run.ErrorMessage)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent run stuck in %s", run.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := answer.Load().(string)
	var joined struct {
		Finished bool                        `json:"finished"`
		State    string                      `json:"state"`
		Results  []agentcore.WorkDatasetItem `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &joined); err != nil || !joined.Finished || joined.State != agentcore.WorkJobStateCompleted || len(joined.Results) != 25 {
		t.Fatalf("parent joined %q", raw)
	}
	jobs, _ := h.store.ListWorkJobs(context.Background(), testApp)
	if len(jobs) != 1 || jobs[0].ParentRunID != parent.ID {
		t.Fatalf("subtask job = %+v", jobs)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	for _, name := range engine.WorkJobDelegationTools {
		if model.workerTools[name] {
			t.Fatalf("worker was offered %s: delegation must stay one level deep", name)
		}
	}
	if !model.workerTools[submitToolName] {
		t.Fatalf("worker tools = %v", model.workerTools)
	}
}

// Browser workers of one agent: a local standard browser is one Chrome per
// profile, so one worker at a time; Extension mode gives each worker a tab
// in the bot's browser, up to three; cloud browsers are separate.
func TestBrowserWorkersFollowBrowserMode(t *testing.T) {
	for _, tc := range []struct {
		backend string
		want    int
	}{{"standard", 1}, {"extension", extensionBrowserTabs}, {"cloud", 4}} {
		t.Run(tc.backend, func(t *testing.T) {
			var mu sync.Mutex
			active, maxActive := 0, 0
			model := &scriptedWorker{script: submitAll, beforeGo: func(batch []string, step int) {
				if step != 0 {
					return
				}
				mu.Lock()
				active++
				maxActive = max(maxActive, active)
				mu.Unlock()
				time.Sleep(60 * time.Millisecond)
				mu.Lock()
				active--
				mu.Unlock()
			}}
			h := newHarness(t, tempDB(t), model)
			h.tools.Register(tools.Definition{Name: "browser_open", Description: "Open a page", RiskLevel: tools.RiskLevelRead,
				InputSchema: map[string]any{"type": "object"}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			})
			agent := agentcore.Agent{AppID: testApp, ID: "browser-" + tc.backend, Name: "Browser", RuntimeKind: agentcore.RuntimeNativeSDK,
				AllowedTools: []string{"browser_open"}, ApprovalMode: "never", ExecutionConfig: json.RawMessage(fmt.Sprintf(`{"browser":{"backend":%q}}`, tc.backend))}
			if err := h.store.CreateAgent(context.Background(), &agent); err != nil {
				t.Fatal(err)
			}
			spec := testSpec()
			spec.MaxConcurrency = 4
			spec.SerializeBrowser = true
			job, err := h.service.Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp, Spec: spec,
				Worker: agentcore.WorkJobWorker{AgentID: agent.ID, Target: testTarget}, Items: items(80)})
			if err != nil {
				t.Fatal(err)
			}
			job = h.waitState(job.ID, 2*time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateFailed)
			if job.State != agentcore.WorkJobStateCompleted || job.Counts.Found != 80 {
				t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
			}
			mu.Lock()
			defer mu.Unlock()
			if maxActive != tc.want {
				t.Fatalf("%s: %d browser workers at once, want %d", tc.backend, maxActive, tc.want)
			}
		})
	}
}

// A parent limited to delegation gets children with no tools of their own:
// an emptied list must not widen to everything the agent may use.
func TestSubtasksNeverGetToolsTheParentLacked(t *testing.T) {
	var answer atomic.Value
	model := &scriptedWorker{script: submitAll}
	model.parent = delegatingParent(5, &answer)
	h := newHarness(t, tempDB(t), model)
	h.tools.Register(tools.Definition{Name: "send_email", Description: "Send an email", Mutating: true, RiskLevel: tools.RiskLevelRoutine,
		InputSchema: map[string]any{"type": "object"}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"sent":true}`), nil
	})
	agent := agentcore.Agent{AppID: testApp, ID: "lead", Name: "Lead", RuntimeKind: agentcore.RuntimeNativeSDK, ApprovalMode: "never",
		AllowedTools: []string{"spawn_subtasks", "subtask_status", "wait_for_subtasks", "cancel_subtasks", "send_email"}}
	if err := h.store.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	parent, err := h.engine.StartRun(context.Background(), engine.StartRunRequest{AppID: testApp, AgentID: "lead", Target: testTarget,
		Instructions: "Summarize the site.", AllowedTools: []string{"spawn_subtasks", "wait_for_subtasks"}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		run, _ := h.store.GetRun(context.Background(), testApp, parent.ID)
		if run.Status == agentcore.RunStatusCompleted {
			break
		}
		if run.Status == agentcore.RunStatusFailed || time.Now().After(deadline) {
			t.Fatalf("parent run %s: %s", run.Status, run.ErrorMessage)
		}
		time.Sleep(20 * time.Millisecond)
	}
	jobs, _ := h.store.ListWorkJobs(context.Background(), testApp)
	if len(jobs) != 1 || jobs[0].Counts.Found != 5 {
		t.Fatalf("subtask job = %+v", jobs)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if model.workerTools["send_email"] {
		t.Fatalf("child was offered send_email, which its parent could not use: %v", model.workerTools)
	}
	if !model.workerTools[submitToolName] {
		t.Fatalf("worker tools = %v", model.workerTools)
	}
}

// A parent collects every subtask result a page at a time, not only the
// first page.
func TestParentCollectsAllSubtaskResultsInPages(t *testing.T) {
	var answer atomic.Value
	var mu sync.Mutex
	collected := map[string]bool{}
	pages := 0
	model := &scriptedWorker{script: submitAll}
	model.parent = delegatingParent(75, &answer, func(lastTool, lastOutput string) *runtime.NativeModelResponse {
		var out struct {
			JobID      string                      `json:"job_id"`
			Results    []agentcore.WorkDatasetItem `json:"results"`
			NextOffset *int                        `json:"next_results_offset"`
		}
		if lastTool != "wait_for_subtasks" || json.Unmarshal([]byte(lastOutput), &out) != nil {
			return nil
		}
		mu.Lock()
		pages++
		for _, item := range out.Results {
			collected[item.ID] = true
		}
		mu.Unlock()
		if out.NextOffset == nil {
			return nil
		}
		raw, _ := json.Marshal(map[string]any{"job_id": out.JobID, "results_offset": *out.NextOffset})
		return &runtime.NativeModelResponse{Message: runtime.NativeMessage{Role: "assistant", Blocks: []runtime.NativeBlock{{Type: "tool_call",
			ToolCallID: fmt.Sprintf("page-%d", *out.NextOffset), ToolName: "wait_for_subtasks", Input: raw}}}}
	})
	h := newHarness(t, tempDB(t), model)
	agent := agentcore.Agent{AppID: testApp, ID: "lead", Name: "Lead", RuntimeKind: agentcore.RuntimeNativeSDK, ApprovalMode: "never",
		AllowedTools: []string{"spawn_subtasks", "subtask_status", "wait_for_subtasks", "cancel_subtasks"}}
	if err := h.store.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	parent, err := h.engine.StartRun(context.Background(), engine.StartRunRequest{AppID: testApp, AgentID: "lead", Target: testTarget, Instructions: "Summarize the site."})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		run, _ := h.store.GetRun(context.Background(), testApp, parent.ID)
		if run.Status == agentcore.RunStatusCompleted {
			break
		}
		if run.Status == agentcore.RunStatusFailed || time.Now().After(deadline) {
			t.Fatalf("parent run %s: %s", run.Status, run.ErrorMessage)
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(collected) != 75 {
		t.Fatalf("parent collected %d of 75 results over %d pages", len(collected), pages)
	}
}

// editableSkills serves one workspace skill whose content the test edits.
type editableSkills struct {
	mu    sync.Mutex
	skill skills.WorkspaceSkill
}

func (l *editableSkills) current() *skills.WorkspaceSkill {
	l.mu.Lock()
	defer l.mu.Unlock()
	skill := l.skill
	return &skill
}
func (l *editableSkills) GetByID(context.Context, string, string) (*skills.WorkspaceSkill, error) {
	return l.current(), nil
}
func (l *editableSkills) GetActiveByKey(context.Context, string, string) (*skills.WorkspaceSkill, error) {
	return l.current(), nil
}

type packageObjects map[string][]byte

func (p packageObjects) GetObject(_ context.Context, key string) ([]byte, error) {
	if raw, ok := p[key]; ok {
		return raw, nil
	}
	return nil, fmt.Errorf("no object %s", key)
}

// houseStyle builds one published version of the test's workspace skill.
func houseStyle(t *testing.T, objects packageObjects, instructions string) skills.WorkspaceSkill {
	t.Helper()
	definition := skills.Definition{Key: "house-style", Title: "House style", Description: "How to write results", Instructions: instructions, SourceKind: skills.SourceWorkspace}
	archive, checksum, filename, err := skills.BuildSkillArchive(definition)
	if err != nil {
		t.Fatal(err)
	}
	version := skills.SkillVersionForBytes(archive)
	key := "skills/skill-1/" + version + "/" + filename
	objects[key] = archive
	return skills.WorkspaceSkill{ID: "skill-1", Key: definition.Key, VersionKey: version, Title: definition.Title, Description: definition.Description,
		SourceKind: skills.SourceWorkspace, Instructions: instructions, PackageObjectKey: key, PackageChecksum: checksum}
}

// Editing a bot while its job runs does not change later workers: they
// keep the instructions and skill versions the job started with.
func TestWorkersKeepTheBotAsItWasWhenTheJobStarted(t *testing.T) {
	objects := packageObjects{}
	lookup := &editableSkills{skill: houseStyle(t, objects, "Write results in the FIRST house style.")}
	registry := skills.NewRegistry()
	registry.SetWorkspaceLookup(lookup)
	packages := skills.NewPackageStoreRegistry()
	if err := packages.Register(testApp, objects); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var once sync.Once
	model := &scriptedWorker{script: submitAll, beforeGo: func(batch []string, step int) {
		if step == 0 && batch[0] != "item-0001" {
			<-gate
		}
	}}
	h := newHarnessWithSkills(t, tempDB(t), model, registry, packages)
	agent := agentcore.Agent{AppID: testApp, ID: "writer", Name: "Writer", RuntimeKind: agentcore.RuntimeNativeSDK,
		SystemPrompt: "You are the FIRST writer.", Skills: []agentcore.SkillRef{{Key: "house-style"}}}
	if err := h.store.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	job, err := h.service.Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp, Spec: testSpec(),
		Worker: agentcore.WorkJobWorker{AgentID: "writer", Target: testTarget}, Items: items(20)})
	if err != nil {
		t.Fatal(err)
	}
	// The bot and its skill change after the job started.
	agent.SystemPrompt = "You are the SECOND writer."
	if err := h.store.UpdateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	edited := houseStyle(t, objects, "Write results in the SECOND house style.")
	lookup.mu.Lock()
	lookup.skill = edited
	lookup.mu.Unlock()
	once.Do(func() { close(gate) })
	job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateFailed)
	if job.State != agentcore.WorkJobStateCompleted {
		for _, attempt := range h.attempts(job.ID) {
			t.Logf("attempt %s %s: %s", attempt.ID, attempt.State, attempt.Outcome)
		}
		t.Fatalf("job = %s/%s %+v %s", job.State, job.WaitingReason, job.Counts, job.Error)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.prompts) != 2 {
		t.Fatalf("worker prompts = %d", len(model.prompts))
	}
	for _, prompt := range model.prompts {
		if !strings.Contains(prompt, "FIRST writer") || !strings.Contains(prompt, "FIRST house style") || strings.Contains(prompt, "SECOND") {
			t.Fatalf("a worker used the edited bot:\n%s", prompt)
		}
	}
}

// A provider that reports no usage leaves the workers' cost unknown, not
// zero: their reservations stay counted against the budget.
func TestUnreportedProviderUsageKeepsReservations(t *testing.T) {
	model := &scriptedWorker{script: submitAll, noUsage: true}
	h := newHarness(t, tempDB(t), model)
	spec := testSpec()
	spec.Budget = agentcore.WorkJobBudget{MaxTokens: 1000, ReserveTokensPerAttempt: 400}
	job := h.create(spec, 20)
	job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateFailed)
	if job.State != agentcore.WorkJobStateCompleted || job.Counts.Found != 20 {
		t.Fatalf("job = %s/%s %+v", job.State, job.WaitingReason, job.Counts)
	}
	if job.Usage.ReservedTokens != 800 {
		t.Fatalf("usage = %+v, want both reservations kept", job.Usage)
	}
	for _, attempt := range h.attempts(job.ID) {
		if !attempt.UsagePending {
			t.Fatalf("attempt %s usage counted as known", attempt.ID)
		}
	}
}

// When every worker is paused for an answer, nothing can proceed: the job
// waits for the user instead of reporting that it is running.
func TestJobWaitsWhenEveryWorkerAsksTheUser(t *testing.T) {
	question := json.RawMessage(`{"questions":[{"id":"which","header":"Which","question":"Which source should I trust?","isOther":true,"options":[{"label":"A"},{"label":"B"}]}]}`)
	model := &scriptedWorker{
		script: func(batch, accepted []string, step int) ([]string, bool) { return nil, false },
		extra: func(batch []string, step int) []runtime.NativeBlock {
			if step != 0 {
				return nil
			}
			return []runtime.NativeBlock{{Type: "tool_call", ToolCallID: "ask-" + batch[0], ToolName: "request_user_input", Input: question}}
		},
	}
	h := newHarness(t, tempDB(t), model)
	agent := agentcore.Agent{AppID: testApp, ID: "asker", Name: "Asker", RuntimeKind: agentcore.RuntimeNativeSDK, ApprovalMode: "never",
		AllowedTools: []string{"request_user_input"}}
	if err := h.store.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.MaxConcurrency = 2
	job, err := h.service.Create(context.Background(), agentcore.CreateWorkJobRequest{AppID: testApp, Spec: spec,
		Worker: agentcore.WorkJobWorker{AgentID: "asker", Target: testTarget}, Items: items(20)})
	if err != nil {
		t.Fatal(err)
	}
	job = h.waitState(job.ID, time.Minute, agentcore.WorkJobStateWaiting, agentcore.WorkJobStateCompleted, agentcore.WorkJobStateFailed)
	if job.WaitingReason != agentcore.WorkJobWaitingWorkerInput || len(job.ActiveRuns) != 2 {
		for _, attempt := range h.attempts(job.ID) {
			run, _ := h.store.GetRun(context.Background(), testApp, attempt.RunID)
			calls, _ := h.store.ListToolCalls(context.Background(), testApp, attempt.RunID)
			for _, call := range calls {
				t.Logf("call %s err=%q out=%.200s", call.ToolName, call.Error, call.Output)
			}
			if run != nil {
				t.Logf("attempt %s %s run %s %s %s", attempt.ID, attempt.Outcome, run.Status, run.PauseReason, run.ErrorMessage)
			}
		}
		t.Fatalf("job = %s/%s active=%v", job.State, job.WaitingReason, job.ActiveRuns)
	}
	for _, runID := range job.ActiveRuns {
		run, _ := h.store.GetRun(context.Background(), testApp, runID)
		if run == nil || run.Status != agentcore.RunStatusPaused {
			t.Fatalf("worker %s = %+v, want paused", runID, run)
		}
	}
	if !slices.Contains(job.ActiveRuns, job.BlockedRunID) {
		t.Fatalf("blocked run %q is not one of the paused workers %v", job.BlockedRunID, job.ActiveRuns)
	}
}
