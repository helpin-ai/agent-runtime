package workjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
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
