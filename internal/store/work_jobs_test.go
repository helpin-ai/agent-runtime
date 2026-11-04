package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func openWorkJobSQL(t *testing.T) *SQL {
	t.Helper()
	s, err := OpenSQL(SQLConfig{Driver: "sqlite", DSN: "file:" + filepath.Join(t.TempDir(), "runtime.db") + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	return s
}

func workItems(n int) []agentcore.WorkItemInput {
	items := make([]agentcore.WorkItemInput, n)
	for i := range items {
		items[i] = agentcore.WorkItemInput{ID: fmt.Sprintf("item-%03d", i+1), Input: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i+1))}
	}
	return items
}

func createWorkJob(t *testing.T, s *SQL, id string, spec agentcore.WorkJobSpec, items []agentcore.WorkItemInput) *agentcore.WorkJob {
	t.Helper()
	job, created, err := s.CreateWorkJob(context.Background(), agentcore.NewWorkJob{AppID: "app-a", ID: id, GroupID: "group-" + id, IdempotencyKey: "key-" + id, Spec: spec, SealedWorker: []byte("sealed"), Items: items})
	if err != nil || !created {
		t.Fatalf("create job: %v created=%v", err, created)
	}
	return job
}

// startWorker claims a batch and links it to a new group member run, as the
// job controller does.
func startWorker(t *testing.T, s *SQL, job *agentcore.WorkJob, attemptID string, batch int) (*agentcore.WorkAttempt, []agentcore.WorkItem) {
	t.Helper()
	ctx := context.Background()
	attempt, items, err := s.ClaimWorkBatch(ctx, agentcore.WorkClaim{AppID: "app-a", JobID: job.ID, AttemptID: attemptID, BatchSize: batch, Reserve: 1000})
	if err != nil || attempt == nil {
		t.Fatalf("claim: %v %v", attempt, err)
	}
	runID := "run-" + attemptID
	if err := createGroupedRun(ctx, s, newGroupRun(runID), job.GroupID); err != nil {
		t.Fatal(err)
	}
	generation, err := s.BumpRunExecutionGeneration(ctx, "app-a", runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkAttemptRun(ctx, "app-a", attemptID, runID, generation); err != nil {
		t.Fatal(err)
	}
	attempt.RunID, attempt.Generation = runID, generation
	return attempt, items
}

func submit(s *SQL, attempt *agentcore.WorkAttempt, itemID, outcome, digest string) (*agentcore.WorkReceipt, error) {
	return s.AcceptWorkResult(context.Background(), agentcore.WorkResult{AppID: "app-a", AttemptID: attempt.ID, RunID: attempt.RunID, ItemID: itemID,
		Outcome: outcome, Result: json.RawMessage(`{"v":"` + digest + `"}`), Digest: digest})
}

func TestWorkJobCreateIsIdempotentAndCreatesHiddenGroup(t *testing.T) {
	ctx := context.Background()
	s := openWorkJobSQL(t)
	job := createWorkJob(t, s, "job-1", agentcore.WorkJobSpec{Objective: "o"}, workItems(25))
	if job.State != agentcore.WorkJobStateQueued || job.Counts.Total != 25 || job.Counts.Pending != 25 {
		t.Fatalf("job = %+v", job)
	}
	again, created, err := s.CreateWorkJob(ctx, agentcore.NewWorkJob{AppID: "app-a", ID: "job-other", GroupID: "group-other", IdempotencyKey: "key-job-1", Items: workItems(3)})
	if err != nil || created || again.ID != job.ID || again.Counts.Total != 25 {
		t.Fatalf("replay = %+v created=%v err=%v", again, created, err)
	}
	group, err := s.GetRunGroup(ctx, "app-a", job.GroupID)
	if err != nil || group.OwnerKind != agentcore.RunGroupOwnerWorkJob {
		t.Fatalf("group = %+v, %v", group, err)
	}
	attempt, _ := startWorker(t, s, job, "att-1", 10)
	runs, err := s.ListRuns(ctx, "app-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.ID == attempt.RunID {
			t.Fatal("job worker visible in default run listing")
		}
	}
}

func TestWorkJobClaimAcceptReplayAndFencing(t *testing.T) {
	ctx := context.Background()
	s := openWorkJobSQL(t)
	job := createWorkJob(t, s, "job-1", agentcore.WorkJobSpec{Objective: "o"}, workItems(25))
	attempt, items := startWorker(t, s, job, "att-1", 10)
	if len(items) != 10 || items[0].ID != "item-001" || items[9].ID != "item-010" {
		t.Fatalf("claimed %v", items)
	}
	second, items2 := startWorker(t, s, job, "att-2", 10)
	if items2[0].ID != "item-011" {
		t.Fatalf("second claim overlapped: %v", items2[0].ID)
	}

	receipt, err := submit(s, attempt, "item-001", agentcore.WorkItemOutcomeCompleted, "a")
	if err != nil || receipt.Status != agentcore.WorkReceiptAccepted || receipt.Remaining != 9 {
		t.Fatalf("receipt = %+v, %v", receipt, err)
	}
	// Identical replay: same receipt, no second write.
	replay, err := submit(s, attempt, "item-001", agentcore.WorkItemOutcomeCompleted, "a")
	if err != nil || replay.Status != agentcore.WorkReceiptAlreadyAccepted {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	var rejected *agentcore.WorkRejectedError
	if _, err := submit(s, attempt, "item-001", agentcore.WorkItemOutcomeNoResult, "b"); !errors.As(err, &rejected) {
		t.Fatalf("replacement of an accepted result = %v", err)
	}
	if _, err := submit(s, attempt, "item-011", agentcore.WorkItemOutcomeCompleted, "c"); !errors.As(err, &rejected) {
		t.Fatalf("item of another batch = %v", err)
	}
	if _, err := submit(s, attempt, "item-999", agentcore.WorkItemOutcomeCompleted, "c"); !errors.As(err, &rejected) {
		t.Fatalf("unknown item = %v", err)
	}
	forged := *second
	forged.RunID = attempt.RunID
	if _, err := submit(s, &forged, "item-011", agentcore.WorkItemOutcomeCompleted, "c"); !errors.As(err, &rejected) {
		t.Fatalf("run submitting for another attempt = %v", err)
	}
	// Recovery grants new execution authority; the old generation is fenced.
	if _, err := s.BumpRunExecutionGeneration(ctx, "app-a", attempt.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := submit(s, attempt, "item-002", agentcore.WorkItemOutcomeCompleted, "d"); !errors.As(err, &rejected) {
		t.Fatalf("stale generation = %v", err)
	}
	stored, _ := s.ListWorkItems(ctx, "app-a", job.ID, "", "", 1)
	if stored.Items[0].Outcome != agentcore.WorkItemOutcomeCompleted || string(stored.Items[0].Result) != `{"v":"a"}` {
		t.Fatalf("accepted result changed: %+v", stored.Items[0])
	}
}

func TestWorkAttemptCloseRetriesUnresolvedAndAppliesUsageOnce(t *testing.T) {
	ctx := context.Background()
	s := openWorkJobSQL(t)
	job := createWorkJob(t, s, "job-1", agentcore.WorkJobSpec{Objective: "o", MaxAttemptsPerItem: 2}, workItems(10))
	attempt, _ := startWorker(t, s, job, "att-1", 10)
	for _, id := range []string{"item-001", "item-002", "item-003"} {
		if _, err := submit(s, attempt, id, agentcore.WorkItemOutcomeCompleted, id); err != nil {
			t.Fatal(err)
		}
	}
	usage := agentcore.WorkAttemptClose{AppID: "app-a", AttemptID: attempt.ID, InputTokens: 100, OutputTokens: 10}
	if err := s.ApplyWorkAttemptUsage(ctx, usage); err != nil {
		t.Fatal(err)
	}
	// Duplicate and stale notifications add nothing.
	if err := s.ApplyWorkAttemptUsage(ctx, usage); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyWorkAttemptUsage(ctx, agentcore.WorkAttemptClose{AppID: "app-a", AttemptID: attempt.ID, InputTokens: 50}); err != nil {
		t.Fatal(err)
	}
	// The worker claims it finished, with seven items unresolved.
	closed, err := s.CloseWorkAttempt(ctx, agentcore.WorkAttemptClose{AppID: "app-a", AttemptID: attempt.ID, State: agentcore.WorkAttemptStateCompleted, InputTokens: 160, OutputTokens: 20})
	if err != nil || closed.State != agentcore.WorkAttemptStatePartial || closed.Accepted != 3 {
		t.Fatalf("closed = %+v, %v", closed, err)
	}
	if _, err := s.CloseWorkAttempt(ctx, agentcore.WorkAttemptClose{AppID: "app-a", AttemptID: attempt.ID, State: agentcore.WorkAttemptStateCompleted, InputTokens: 500}); err != nil {
		t.Fatal(err)
	}
	job, _ = s.GetWorkJob(ctx, "app-a", job.ID)
	if job.Usage.InputTokens != 160 || job.Usage.OutputTokens != 20 || job.Usage.ReservedTokens != 0 {
		t.Fatalf("usage = %+v", job.Usage)
	}
	if job.Counts.Completed != 3 || job.Counts.RetryWait != 7 {
		t.Fatalf("counts = %+v", job.Counts)
	}
	// A late submission for a closed attempt is rejected; accepted work stays.
	var rejected *agentcore.WorkRejectedError
	if _, err := submit(s, attempt, "item-004", agentcore.WorkItemOutcomeCompleted, "late"); !errors.As(err, &rejected) {
		t.Fatalf("late submission = %v", err)
	}
	// The retry assigns only the unresolved items to a fresh attempt.
	retry, items := startWorker(t, s, job, "att-2", 10)
	if len(items) != 7 || items[0].ID != "item-004" {
		t.Fatalf("retry claimed %v", items)
	}
	if _, err := s.CloseWorkAttempt(ctx, agentcore.WorkAttemptClose{AppID: "app-a", AttemptID: retry.ID, State: agentcore.WorkAttemptStateFailed}); err != nil {
		t.Fatal(err)
	}
	job, _ = s.GetWorkJob(ctx, "app-a", job.ID)
	if job.Counts.Failed != 7 {
		t.Fatalf("items past max attempts must fail, counts = %+v", job.Counts)
	}
	noProgress, _, err := s.WorkJobProgress(ctx, "app-a", job.ID)
	if err != nil || noProgress != 1 {
		t.Fatalf("no progress = %d, %v", noProgress, err)
	}
}

func TestWorkRetryableExceptionSchedulesRetryUntilAttemptsExhausted(t *testing.T) {
	ctx := context.Background()
	s := openWorkJobSQL(t)
	job := createWorkJob(t, s, "job-1", agentcore.WorkJobSpec{Objective: "o", MaxAttemptsPerItem: 2}, workItems(1))
	exception := &agentcore.WorkItemException{Kind: "site_unreachable", Detail: "timeout"}
	// The retry submits identical content: it is a new result, not a replay.
	for i, want := range []string{agentcore.WorkReceiptRetryScheduled, agentcore.WorkReceiptAccepted} {
		attempt, _ := startWorker(t, s, job, fmt.Sprintf("att-%d", i), 10)
		receipt, err := s.AcceptWorkResult(ctx, agentcore.WorkResult{AppID: "app-a", AttemptID: attempt.ID, RunID: attempt.RunID, ItemID: "item-001",
			Outcome: agentcore.WorkItemOutcomeException, Exception: exception, Digest: "same", Retryable: true})
		if err != nil || receipt.Status != want {
			t.Fatalf("attempt %d receipt = %+v, %v", i, receipt, err)
		}
		if _, err := s.CloseWorkAttempt(ctx, agentcore.WorkAttemptClose{AppID: "app-a", AttemptID: attempt.ID, State: agentcore.WorkAttemptStateCompleted}); err != nil {
			t.Fatal(err)
		}
	}
	job, _ = s.GetWorkJob(ctx, "app-a", job.ID)
	if job.Counts.Exceptions != 1 || job.Counts.NoResult != 0 {
		t.Fatalf("an unreachable site must end as an exception, never no_result: %+v", job.Counts)
	}
}

func TestWorkClaimRespectsBudgetStateAndDependencies(t *testing.T) {
	ctx := context.Background()
	s := openWorkJobSQL(t)
	items := workItems(3)
	items[2].DependsOn = []string{"item-001"}
	job := createWorkJob(t, s, "job-1", agentcore.WorkJobSpec{Objective: "o", Budget: agentcore.WorkJobBudget{MaxTokens: 1500}}, items)
	attempt, claimed := startWorker(t, s, job, "att-1", 10)
	if len(claimed) != 2 || claimed[1].ID != "item-002" {
		t.Fatalf("dependency not respected: %v", claimed)
	}
	// 1000 reserved of 1500: a second reservation of 1000 is not admitted.
	if next, _, err := s.ClaimWorkBatch(ctx, agentcore.WorkClaim{AppID: "app-a", JobID: job.ID, AttemptID: "att-x", BatchSize: 10, Reserve: 1000}); err != nil || next != nil {
		t.Fatalf("over-budget claim = %v, %v", next, err)
	}
	if _, err := submit(s, attempt, "item-001", agentcore.WorkItemOutcomeCompleted, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := submit(s, attempt, "item-002", agentcore.WorkItemOutcomeCompleted, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseWorkAttempt(ctx, agentcore.WorkAttemptClose{AppID: "app-a", AttemptID: attempt.ID, State: agentcore.WorkAttemptStateCompleted, InputTokens: 200}); err != nil {
		t.Fatal(err)
	}
	paused := agentcore.WorkJobStatePaused
	if _, err := s.UpdateWorkJob(ctx, "app-a", job.ID, agentcore.WorkJobUpdate{State: &paused}); err != nil {
		t.Fatal(err)
	}
	if next, _, err := s.ClaimWorkBatch(ctx, agentcore.WorkClaim{AppID: "app-a", JobID: job.ID, AttemptID: "att-y", BatchSize: 10, Reserve: 100}); err != nil || next != nil {
		t.Fatalf("paused job claim = %v, %v", next, err)
	}
	running := agentcore.WorkJobStateRunning
	if _, err := s.UpdateWorkJob(ctx, "app-a", job.ID, agentcore.WorkJobUpdate{State: &running}); err != nil {
		t.Fatal(err)
	}
	_, claimed = startWorker(t, s, job, "att-2", 10)
	if len(claimed) != 1 || claimed[0].ID != "item-003" {
		t.Fatalf("dependent item after its prerequisite = %v", claimed)
	}
}

func TestWorkClaimsFromConcurrentWorkersAreDisjoint(t *testing.T) {
	ctx := context.Background()
	s := openWorkJobSQL(t)
	job := createWorkJob(t, s, "job-1", agentcore.WorkJobSpec{Objective: "o"}, workItems(200))
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]string{}
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			attempt, items, err := s.ClaimWorkBatch(ctx, agentcore.WorkClaim{AppID: "app-a", JobID: job.ID, AttemptID: fmt.Sprintf("att-%02d", i), BatchSize: 7})
			if err != nil {
				errs <- err
				return
			}
			if attempt == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, item := range items {
				if owner, ok := seen[item.ID]; ok {
					errs <- fmt.Errorf("%s claimed by %s and %s", item.ID, owner, attempt.ID)
				}
				seen[item.ID] = attempt.ID
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(seen) != 200 {
		t.Fatalf("claimed %d of 200 items", len(seen))
	}
}

func TestWorkDatasetPublicationIsVersionedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openWorkJobSQL(t)
	job := createWorkJob(t, s, "job-1", agentcore.WorkJobSpec{Objective: "o"}, workItems(3))
	attempt, _ := startWorker(t, s, job, "att-1", 10)
	if _, err := submit(s, attempt, "item-002", agentcore.WorkItemOutcomeCompleted, "b"); err != nil {
		t.Fatal(err)
	}
	first, err := s.PublishWorkJobDataset(ctx, "app-a", job.ID, time.Time{})
	if err != nil || first.Revision != 1 || len(first.Items) != 1 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	same, err := s.PublishWorkJobDataset(ctx, "app-a", job.ID, time.Time{})
	if err != nil || same.Revision != 1 || same.Digest != first.Digest {
		t.Fatalf("unchanged publication created a revision: %+v, %v", same, err)
	}
	if _, err := submit(s, attempt, "item-001", agentcore.WorkItemOutcomeNoResult, "a"); err != nil {
		t.Fatal(err)
	}
	second, err := s.PublishWorkJobDataset(ctx, "app-a", job.ID, time.Time{})
	if err != nil || second.Revision != 2 || len(second.Items) != 2 || second.Items[0].ID != "item-001" {
		t.Fatalf("second = %+v, %v", second, err)
	}
	latest, err := s.GetWorkJobDataset(ctx, "app-a", job.ID)
	if err != nil || latest.Revision != 2 {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	events, err := s.ListWorkJobEvents(ctx, "app-a", job.ID, 0, 0)
	if err != nil || len(events.Items) == 0 || events.Items[0].Type != "work_job.created" {
		t.Fatalf("events = %+v, %v", events, err)
	}
	for i, event := range events.Items {
		if event.Seq != int64(i+1) {
			t.Fatalf("event sequence gap at %d: %+v", i, event)
		}
	}
}
