package memoryjobs

import (
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testService(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "jobs.db"), map[string]Callback{"app": {server.URL, "secret"}, "other": {server.URL, "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func testJob(kind string) sdk.MemoryLearningJob {
	return sdk.MemoryLearningJob{AppID: "app", BankID: "bank", Kind: kind, SourceID: "source", Revision: "one"}
}
func allow(t *testing.T, s *Service) {
	t.Helper()
	if err := s.SetPolicy(sdk.MemoryLearningPolicy{AppID: "app", AllowExtraction: true, AllowConsolidation: true}); err != nil {
		t.Fatal(err)
	}
}
func oneJob(t *testing.T, s *Service) sdk.MemoryLearningJob {
	t.Helper()
	jobs, err := s.Jobs(context.Background(), "app", "bank")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	return jobs[0]
}

func TestJobsIdempotenceIsolationAndExtractionBeforeConsolidation(t *testing.T) {
	var calls atomic.Int32
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("callback not authenticated")
		}
		calls.Add(1)
		var j sdk.MemoryLearningJob
		json.NewDecoder(r.Body).Decode(&j)
		out := sdk.MemoryLearningResult{Facts: 2}
		if j.Kind == "consolidation" {
			out = sdk.MemoryLearningResult{Processed: 2, Created: 1}
		}
		json.NewEncoder(w).Encode(out)
	})
	ctx := context.Background()
	job, err := s.Submit(ctx, testJob("extraction"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(ctx, testJob("extraction")); err != nil {
		t.Fatal(err)
	}
	if jobs, err := s.Jobs(ctx, "other", "bank"); err != nil || len(jobs) != 0 {
		t.Fatalf("foreign jobs=%+v err=%v", jobs, err)
	}
	if ran, err := s.step(ctx, "extraction"); ran || err != nil {
		t.Fatalf("ran without idle permission: %v %v", ran, err)
	}
	allow(t, s)
	if _, err = s.Submit(ctx, testJob("consolidation")); err != nil {
		t.Fatal(err)
	}
	if ran, err := s.step(ctx, "consolidation"); ran || err != nil {
		t.Fatalf("consolidated before extraction: %v %v", ran, err)
	}
	if ran, err := s.step(ctx, "extraction"); !ran || err != nil {
		t.Fatalf("extraction: %v %v", ran, err)
	}
	if _, err = s.Submit(ctx, testJob("extraction")); err != nil {
		t.Fatal(err)
	}
	if ran, _ := s.step(ctx, "extraction"); ran {
		t.Fatal("duplicate revision reprocessed")
	}
	if ran, err := s.step(ctx, "consolidation"); !ran || err != nil {
		t.Fatalf("consolidation: %v %v", ran, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if err = s.Retry(ctx, "other", job.ID); err == nil {
		t.Fatal("foreign app retried job")
	}
}

func TestJobsBoundedRetriesAndExplicitRetry(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(sdk.MemoryLearningResult{Error: "provider secret unavailable"})
	})
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	j := testJob("extraction")
	if _, err := s.Submit(ctx, j); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		allow(t, s)
		ran, err := s.step(ctx, "extraction")
		if !ran || err != nil {
			t.Fatalf("attempt %d: %v %v", attempt, ran, err)
		}
		job := oneJob(t, s)
		if job.Attempts != attempt || job.Error != "provider [redacted] unavailable" {
			t.Fatalf("job=%+v", job)
		}
		if attempt < 3 {
			if ran, _ = s.step(ctx, "extraction"); ran {
				t.Fatal("retry backoff ignored")
			}
			now = *job.RetryAt
		} else if job.State != "failed" {
			t.Fatalf("state=%s", job.State)
		}
	}
	if ran, _ := s.step(ctx, "extraction"); ran {
		t.Fatal("retry budget exceeded")
	}
	job := oneJob(t, s)
	if err := s.Retry(ctx, "app", job.ID); err != nil {
		t.Fatal(err)
	}
	if job = oneJob(t, s); job.Attempts != 0 || job.State != "queued" {
		t.Fatalf("retry=%+v", job)
	}
}

func TestJobsForegroundInterruptionDoesNotConsumeRetry(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	ctx := context.Background()
	allow(t, s)
	s.Submit(ctx, testJob("extraction"))
	done := make(chan error, 1)
	go func() { _, err := s.step(ctx, "extraction"); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("callback never started")
	}
	s.SetPolicy(sdk.MemoryLearningPolicy{AppID: "app"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("foreground failed to interrupt learning")
	}
	close(release)
	if job := oneJob(t, s); job.State != "queued" || job.Attempts != 0 || job.RetryAt != nil {
		t.Fatalf("interrupted job=%+v", job)
	}
}

func TestJobsRestartRecoveryAndSingleOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	callbacks := map[string]Callback{"app": {"http://127.0.0.1:1", "secret"}}
	ctx := context.Background()
	s, err := Open(ctx, path, callbacks)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(ctx, path, callbacks); err == nil {
		other.Close()
		t.Fatal("second worker admitted")
	}
	for i := 0; i < extractionConcurrency; i++ {
		job := testJob("extraction")
		job.SourceID = fmt.Sprintf("source-%d", i)
		j, err := s.Submit(ctx, job)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.db.Exec(`UPDATE memory_learning_jobs SET state='extracting',payload=json_set(payload,'$.state','extracting'),lease='old',lease_until=? WHERE id=?`, time.Now().Add(time.Hour).UnixMilli(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(ctx, path, callbacks)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	jobs, err := s.Jobs(ctx, "app", "bank")
	if err != nil || len(jobs) != extractionConcurrency {
		t.Fatalf("recovered jobs=%+v err=%v", jobs, err)
	}
	for _, job := range jobs {
		if job.State != "queued" || job.Attempts != 0 {
			t.Fatalf("recovered=%+v", job)
		}
	}
	if ran, _ := s.step(ctx, "extraction"); ran {
		t.Fatal("restart reused expired host permission")
	}
}

func TestJobsRevisionChangeFencesOldWriter(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		json.NewEncoder(w).Encode(sdk.MemoryLearningResult{Facts: 2})
	})
	ctx := context.Background()
	allow(t, s)
	s.Submit(ctx, testJob("extraction"))
	done := make(chan error, 1)
	go func() { _, err := s.step(ctx, "extraction"); done <- err }()
	<-started
	updated := testJob("extraction")
	updated.Revision = "two"
	if _, err := s.Submit(ctx, updated); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if job := oneJob(t, s); job.Revision != "two" || job.State != "queued" || job.Result.Facts != 0 {
		t.Fatalf("stale writer won: %+v", job)
	}
}

func TestJobsCooldownMigrationAndInvalidSource(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(sdk.MemoryLearningResult{InvalidSource: true})
	})
	ctx := context.Background()
	allow(t, s)
	j := testJob("consolidation")
	now := time.Now()
	j.LastSuccess = &now
	s.Submit(ctx, j)
	if ran, _ := s.step(ctx, "consolidation"); ran {
		t.Fatal("migration lost cooldown")
	}
	j = testJob("extraction")
	s.Submit(ctx, j)
	if ran, err := s.step(ctx, "extraction"); !ran || err != nil {
		t.Fatalf("invalid source %v %v", ran, err)
	}
	jobs, _ := s.Jobs(ctx, "app", "bank")
	for _, job := range jobs {
		if job.Kind == "extraction" && (job.State != "cancelled" || job.Attempts != 0) {
			t.Fatalf("source=%+v", job)
		}
	}
}

func TestJobsManualConsolidationAndGracefulShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		json.NewEncoder(w).Encode(sdk.MemoryLearningResult{Processed: 1})
	})
	ctx := context.Background()
	policy := sdk.MemoryLearningPolicy{AppID: "app", AllowExtraction: true}
	s.SetPolicy(policy)
	j := testJob("consolidation")
	j.Manual = true
	now := time.Now()
	j.LastSuccess = &now
	s.Submit(ctx, j)
	done := make(chan error, 1)
	go func() { _, err := s.step(ctx, "consolidation"); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("manual consolidation did not bypass delay/cooldown")
	}
	s.SetPolicy(policy)
	select {
	case err := <-done:
		t.Fatalf("manual work incorrectly cancelled: %v", err)
	default:
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not interrupt work")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestJobsParallelExtractionBoundAndConsolidationOrder(t *testing.T) {
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	var active, peak, completed atomic.Int32
	var seen sync.Map
	consolidated := make(chan struct{}, 1)
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		var j sdk.MemoryLearningJob
		if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
			t.Error(err)
			return
		}
		if _, duplicate := seen.LoadOrStore(j.ID, true); duplicate {
			t.Error("job claimed twice")
		}
		if j.Kind == "consolidation" {
			if active.Load() != 0 || completed.Load() != 7 {
				t.Errorf("consolidation overlapped or preceded extraction: active=%d completed=%d", active.Load(), completed.Load())
			}
			json.NewEncoder(w).Encode(sdk.MemoryLearningResult{Processed: 7, Created: 1})
			consolidated <- struct{}{}
			return
		}
		count := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); count > old; old = peak.Load() {
			if peak.CompareAndSwap(old, count) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		completed.Add(1)
		json.NewEncoder(w).Encode(sdk.MemoryLearningResult{Facts: 1})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	allow(t, s)
	for i := 0; i < 7; i++ {
		j := testJob("extraction")
		j.SourceID = fmt.Sprintf("source-%d", i)
		if _, err := s.Submit(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Submit(ctx, testJob("consolidation")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for i := 0; i < extractionConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("extractions did not run concurrently")
		}
	}
	jobs, err := s.Jobs(ctx, "app", "bank")
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	running := 0
	for _, j := range jobs {
		if j.State == "extracting" {
			running++
		}
	}
	if running != extractionConcurrency {
		close(release)
		t.Fatalf("running=%d", running)
	}
	close(release)
	select {
	case <-consolidated:
	case <-time.After(5 * time.Second):
		t.Fatal("queue failed to drain and consolidate")
	}
	if peak.Load() != extractionConcurrency {
		t.Fatalf("peak concurrency=%d", peak.Load())
	}
}

func TestJobsParallelInterruptionReturnsEveryJobToQueue(t *testing.T) {
	started := make(chan struct{}, extractionConcurrency)
	release := make(chan struct{})
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	ctx := context.Background()
	allow(t, s)
	for i := 0; i < extractionConcurrency; i++ {
		j := testJob("extraction")
		j.SourceID = fmt.Sprintf("source-%d", i)
		if _, err := s.Submit(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	for i := 0; i < extractionConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("parallel callbacks did not start")
		}
	}
	s.SetPolicy(sdk.MemoryLearningPolicy{AppID: "app"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		jobs, err := s.Jobs(ctx, "app", "bank")
		if err != nil {
			t.Fatal(err)
		}
		queued := 0
		for _, j := range jobs {
			if j.State == "queued" && j.Attempts == 0 && j.RetryAt == nil {
				queued++
			}
		}
		if queued == extractionConcurrency {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("interrupted jobs not returned to queue: %+v", jobs)
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not shut down")
	}
}
