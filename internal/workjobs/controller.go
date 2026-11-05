package workjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/id"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// credentialMinValidity is how long a copied model credential must still be
// valid when a worker starts; shorter-lived ones are refreshed first.
const credentialMinValidity = 10 * time.Minute

// Run reconciles and dispatches until ctx ends. The first pass is recovery:
// it attaches to every persisted attempt and continues interrupted workers
// from their checkpoints before any new dispatch. Generic lightweight
// recovery never runs work-job workers, so this is their only owner.
func (s *Service) Run(ctx context.Context) {
	if s.store == nil {
		return
	}
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

// Tick runs one reconcile and dispatch pass over every active job.
// Dispatch is round-robin: each job starts at most one batch per round, so
// one large job cannot take every worker slot of its app.
func (s *Service) Tick(ctx context.Context) {
	if s.store == nil {
		return
	}
	jobs, err := s.store.ListActiveWorkJobs(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "list work jobs failed", "error", err)
		return
	}
	sched := s.schedule(ctx, jobs)
	var more []int
	open := make([]int, len(jobs))
	for i := range jobs {
		if ctx.Err() != nil {
			return
		}
		again, count, err := s.step(ctx, &jobs[i], sched)
		if err != nil {
			slog.ErrorContext(ctx, "work job step failed", "app_id", jobs[i].AppID, "job_id", jobs[i].ID, "error", err)
		}
		open[i] = count
		if again {
			more = append(more, i)
		}
	}
	for len(more) > 0 && ctx.Err() == nil {
		var next []int
		for _, i := range more {
			s.mu.Lock()
			started, err := s.dispatch(ctx, &jobs[i], open[i], sched, 1)
			s.mu.Unlock()
			if err != nil {
				slog.ErrorContext(ctx, "work job dispatch failed", "app_id", jobs[i].AppID, "job_id", jobs[i].ID, "error", err)
				continue
			}
			open[i] += started
			if started > 0 {
				next = append(next, i)
			}
		}
		more = next
	}
}

// schedule tracks shared worker capacity during one tick.
type schedule struct {
	// browsers counts open browser attempts per agent across all jobs: a
	// local browser profile belongs to one agent, so two of its workers must
	// never drive it at once.
	browsers map[string]int
	// workers counts open attempts per app, bounded by MaxWorkersPerApp.
	workers map[string]int
	max     int
}

func (s *Service) schedule(ctx context.Context, jobs []agentcore.WorkJob) *schedule {
	sched := &schedule{browsers: map[string]int{}, workers: map[string]int{}, max: s.cfg.MaxWorkersPerApp}
	for _, job := range jobs {
		attempts, err := s.store.ListWorkAttempts(ctx, job.AppID, job.ID, true)
		if err != nil || len(attempts) == 0 {
			continue
		}
		sched.workers[job.AppID] += len(attempts)
		worker, err := s.worker(ctx, job.AppID, job.ID)
		if err != nil || !s.usesBrowser(ctx, job.AppID, worker) {
			continue
		}
		sched.browsers[job.AppID+"\x00"+worker.AgentID] += len(attempts)
	}
	return sched
}

// step reconciles one job and starts at most one batch. It reports whether
// the job could start another batch this tick, and its open attempts.
func (s *Service) step(ctx context.Context, job *agentcore.WorkJob, sched *schedule) (bool, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok, _ := s.Available(job.AppID); !ok {
		// Jobs of a disabled app stay inspectable and untouched; their
		// workers are not executed by anyone until the app is enabled again.
		if job.State != agentcore.WorkJobStateWaiting || job.WaitingReason != agentcore.WorkJobWaitingUnavailable {
			return false, 0, s.setState(ctx, job, agentcore.WorkJobStateWaiting, agentcore.WorkJobWaitingUnavailable, "")
		}
		return false, 0, nil
	}
	open, blocked, err := s.reconcileAttempts(ctx, job)
	if err != nil {
		return false, open, err
	}
	current, err := s.store.GetWorkJob(ctx, job.AppID, job.ID)
	if err != nil {
		return false, open, err
	}
	*job = *current
	switch job.State {
	case agentcore.WorkJobStatePaused:
		return false, open, nil
	case agentcore.WorkJobStateWaiting:
		switch job.WaitingReason {
		case agentcore.WorkJobWaitingBudget, agentcore.WorkJobWaitingNoProgress, agentcore.WorkJobWaitingExceptions:
			// A user decision is needed; Resume clears these.
			return false, open, nil
		case agentcore.WorkJobWaitingWorkerInput, agentcore.WorkJobWaitingWorkerAuth:
			if blocked != nil && open == 1 {
				return false, open, nil
			}
		}
		if err := s.setState(ctx, job, agentcore.WorkJobStateRunning, "", ""); err != nil {
			return false, open, err
		}
	case agentcore.WorkJobStateVerifying:
		return false, open, s.finish(ctx, job)
	}
	started, err := s.dispatch(ctx, job, open, sched, 1)
	return started > 0, open + started, err
}

// reconcileAttempts settles every open attempt against its worker run and
// returns how many stay open and a worker waiting for the user, if any.
func (s *Service) reconcileAttempts(ctx context.Context, job *agentcore.WorkJob) (int, *agentcore.AgentRun, error) {
	attempts, err := s.store.ListWorkAttempts(ctx, job.AppID, job.ID, true)
	if err != nil {
		return 0, nil, err
	}
	open := 0
	var blocked *agentcore.AgentRun
	for _, attempt := range attempts {
		run, err := s.attachRun(ctx, job, attempt)
		if err != nil {
			slog.WarnContext(ctx, "work attempt has no worker run yet", "job_id", job.ID, "attempt_id", attempt.ID, "error", err)
			open++
			continue
		}
		switch {
		case run == nil:
			s.closeAttempt(ctx, job.AppID, attempt, agentcore.WorkAttemptStateFailed, "worker run not found")
		case agentcore.IsTerminalStatus(run.Status):
			state := agentcore.WorkAttemptStateCompleted
			outcome := ""
			switch run.Status {
			case agentcore.RunStatusFailed:
				state, outcome = agentcore.WorkAttemptStateFailed, run.ErrorMessage
			case agentcore.RunStatusCancelled:
				state, outcome = agentcore.WorkAttemptStateCancelled, "worker cancelled"
			}
			s.closeAttempt(ctx, job.AppID, attempt, state, outcome)
		case run.Status == agentcore.RunStatusPaused:
			open++
			blocked = run
			s.applyUsage(ctx, job.AppID, attempt)
		default:
			open++
			if !s.cfg.Engine.IsExecuting(job.AppID, run.ID) {
				s.recoverWorker(ctx, job, attempt, run)
			} else {
				s.applyUsage(ctx, job.AppID, attempt)
			}
		}
	}
	if blocked != nil && open == 1 && job.State != agentcore.WorkJobStatePaused {
		reason := agentcore.WorkJobWaitingWorkerInput
		if blocked.PauseReason == agentcore.PauseReasonAuth {
			reason = agentcore.WorkJobWaitingWorkerAuth
		}
		if job.State != agentcore.WorkJobStateWaiting || job.WaitingReason != reason || job.BlockedRunID != blocked.ID {
			if err := s.setState(ctx, job, agentcore.WorkJobStateWaiting, reason, blocked.ID); err != nil {
				return open, blocked, err
			}
		}
	}
	return open, blocked, nil
}

// attachRun returns an attempt's worker run, creating it with the attempt's
// dispatch key when a crash left the claim without a run. The key makes the
// creation idempotent: a run created before the crash is returned, not
// duplicated.
func (s *Service) attachRun(ctx context.Context, job *agentcore.WorkJob, attempt agentcore.WorkAttempt) (*agentcore.AgentRun, error) {
	if attempt.RunID != "" {
		return s.store.GetRun(ctx, job.AppID, attempt.RunID)
	}
	items, err := s.store.GetWorkItems(ctx, job.AppID, job.ID, attempt.ItemIDs)
	if err != nil {
		return nil, err
	}
	run, err := s.startWorker(ctx, job, attempt, items)
	if err != nil {
		return nil, err
	}
	refreshed, err := s.store.GetWorkAttempt(ctx, job.AppID, attempt.ID)
	if err != nil {
		return nil, err
	}
	if refreshed != nil && refreshed.RunID == "" {
		// The run existed already; its creation hook did not run again.
		member, err := s.store.GetRunGroupMember(ctx, job.AppID, run.ID)
		if err != nil || member == nil {
			return nil, fmt.Errorf("worker run %s has no membership: %v", run.ID, err)
		}
		if err := s.store.SetWorkAttemptRun(ctx, job.AppID, attempt.ID, run.ID, member.ExecutionGeneration); err != nil {
			return nil, err
		}
	}
	return s.store.GetRun(ctx, job.AppID, run.ID)
}

// recoverWorker continues an interrupted worker. It first grants the run a
// new execution generation, so a result from any older execution of the
// same run is fenced off, then resumes it from its checkpoint.
func (s *Service) recoverWorker(ctx context.Context, job *agentcore.WorkJob, attempt agentcore.WorkAttempt, run *agentcore.AgentRun) {
	generation, err := s.store.BumpRunExecutionGeneration(ctx, job.AppID, run.ID)
	if err != nil {
		slog.ErrorContext(ctx, "fence work job worker failed", "job_id", job.ID, "run_id", run.ID, "error", err)
		return
	}
	if err := s.store.SetWorkAttemptRun(ctx, job.AppID, attempt.ID, run.ID, generation); err != nil {
		slog.ErrorContext(ctx, "link recovered worker failed", "job_id", job.ID, "run_id", run.ID, "error", err)
		return
	}
	if _, err := s.cfg.Engine.RecoverWorkJobRun(ctx, job.AppID, run.ID); err != nil {
		slog.ErrorContext(ctx, "recover work job worker failed", "job_id", job.ID, "run_id", run.ID, "error", err)
		return
	}
	_ = s.store.AppendWorkJobEvent(ctx, job.AppID, job.ID, "work_job.attempt_recovered", map[string]interface{}{"attempt_id": attempt.ID, "run_id": run.ID, "generation": generation})
}

// dispatch starts up to limit batches within the job's concurrency, the
// app's worker cap and the browser lease, and returns how many it started.
// With nothing open and nothing to start, it decides why: verification,
// budget, retry backoff or unmet dependencies.
func (s *Service) dispatch(ctx context.Context, job *agentcore.WorkJob, open int, sched *schedule, limit int) (int, error) {
	if job.State != agentcore.WorkJobStateQueued && job.State != agentcore.WorkJobStateRunning {
		return 0, nil
	}
	noProgress, acceptedExceptions, err := s.store.WorkJobProgress(ctx, job.AppID, job.ID)
	if err != nil {
		return 0, err
	}
	if noProgress >= job.Spec.MaxNoProgressAttempts && open == 0 {
		return 0, s.setState(ctx, job, agentcore.WorkJobStateWaiting, agentcore.WorkJobWaitingNoProgress, "")
	}
	worker, err := s.worker(ctx, job.AppID, job.ID)
	if err != nil {
		return 0, s.fail(ctx, job, err.Error())
	}
	usesBrowser := s.usesBrowser(ctx, job.AppID, worker)
	leaseKey := job.AppID + "\x00" + worker.AgentID
	started := 0
	capped := false
	for open < job.Spec.MaxConcurrency && started < limit {
		if usesBrowser && (job.Spec.SerializeBrowser || job.Spec.MaxConcurrency > 1) && sched.browsers[leaseKey] > 0 {
			capped = true
			break
		}
		if sched.max > 0 && sched.workers[job.AppID] >= sched.max {
			capped = true
			break
		}
		if noProgress >= job.Spec.MaxNoProgressAttempts {
			break
		}
		attempt, items, err := s.store.ClaimWorkBatch(ctx, agentcore.WorkClaim{
			AppID: job.AppID, JobID: job.ID, AttemptID: id.New("attempt"), BatchSize: s.batchSize(job), Reserve: job.Spec.Budget.ReserveTokensPerAttempt,
			UsesBrowser: usesBrowser, Now: s.now(),
		})
		if err != nil {
			return started, err
		}
		if attempt == nil {
			break
		}
		open++
		started++
		sched.workers[job.AppID]++
		if usesBrowser {
			sched.browsers[leaseKey]++
		}
		if _, err := s.startWorker(ctx, job, *attempt, items); err != nil {
			var authErr *modelauth.AuthenticationError
			s.closeAttempt(ctx, job.AppID, *attempt, agentcore.WorkAttemptStateCancelled, "worker could not start: "+err.Error())
			sched.workers[job.AppID]--
			if errors.As(err, &authErr) {
				return started - 1, s.setState(ctx, job, agentcore.WorkJobStateWaiting, agentcore.WorkJobWaitingWorkerAuth, "")
			}
			return started - 1, s.fail(ctx, job, "worker could not start: "+err.Error())
		}
	}
	if open > 0 || started > 0 || capped {
		return started, nil
	}
	// Nothing is running and nothing could be claimed: decide why.
	if failed, err := s.store.FailBlockedWorkItems(ctx, job.AppID, job.ID, s.now()); err != nil {
		return 0, err
	} else if failed > 0 {
		s.Wake()
		return 0, nil
	}
	current, err := s.store.GetWorkJob(ctx, job.AppID, job.ID)
	if err != nil {
		return 0, err
	}
	counts := current.Counts
	switch {
	case counts.Pending+counts.Running+counts.RetryWait+counts.Blocked == 0:
		if err := s.setState(ctx, current, agentcore.WorkJobStateVerifying, "", ""); err != nil {
			return 0, err
		}
		return 0, s.finishWith(ctx, current, acceptedExceptions)
	case job.Spec.Budget.MaxTokens > 0 && current.Usage.InputTokens+current.Usage.OutputTokens+current.Usage.ReservedTokens+job.Spec.Budget.ReserveTokensPerAttempt > job.Spec.Budget.MaxTokens:
		return 0, s.setState(ctx, current, agentcore.WorkJobStateWaiting, agentcore.WorkJobWaitingBudget, "")
	case counts.Pending == 0 && counts.RetryWait > 0:
		// Retry backoff: the next tick after the earliest retry time claims.
		return 0, nil
	case counts.Pending > 0:
		return 0, s.setState(ctx, current, agentcore.WorkJobStateWaiting, agentcore.WorkJobWaitingDependencies, "")
	}
	return 0, nil
}

// batchSize shrinks the batch when the job's typical item input would push
// one worker's request over the input budget.
func (s *Service) batchSize(job *agentcore.WorkJob) int {
	size := job.Spec.BatchSize
	page, err := s.store.ListWorkItems(context.Background(), job.AppID, job.ID, agentcore.WorkItemStatePending, "", size)
	if err != nil || len(page.Items) == 0 {
		return size
	}
	total := 0
	for _, item := range page.Items {
		total += len(item.Input)
	}
	average := max(1, total/len(page.Items))
	return max(1, min(size, maxBatchInputBytes/average))
}

func (s *Service) finish(ctx context.Context, job *agentcore.WorkJob) error {
	_, acceptedExceptions, err := s.store.WorkJobProgress(ctx, job.AppID, job.ID)
	if err != nil {
		return err
	}
	return s.finishWith(ctx, job, acceptedExceptions)
}

// finishWith is final verification: every item is accounted for, the
// exception policy holds (or the user accepted the current exceptions) and
// the dataset publishes. Otherwise the job waits for a decision.
func (s *Service) finishWith(ctx context.Context, job *agentcore.WorkJob, acceptedExceptions int) error {
	current, err := s.store.GetWorkJob(ctx, job.AppID, job.ID)
	if err != nil {
		return err
	}
	counts := current.Counts
	if counts.Pending+counts.Running+counts.RetryWait+counts.Blocked > 0 {
		return s.setState(ctx, current, agentcore.WorkJobStateRunning, "", "")
	}
	unresolved := counts.Exceptions + counts.Failed
	ratio := 0.0
	if counts.Total > 0 {
		ratio = float64(unresolved) / float64(counts.Total)
	}
	if ratio > *current.Spec.MaxExceptionRatio && unresolved > acceptedExceptions {
		if _, err := s.store.PublishWorkJobDataset(ctx, current.AppID, current.ID, s.now()); err != nil {
			return err
		}
		return s.setState(ctx, current, agentcore.WorkJobStateWaiting, agentcore.WorkJobWaitingExceptions, "")
	}
	if _, err := s.store.PublishWorkJobDataset(ctx, current.AppID, current.ID, s.now()); err != nil {
		// A failed publication keeps the previous dataset; verification
		// retries on the next tick.
		return err
	}
	return s.setState(ctx, current, agentcore.WorkJobStateCompleted, "", "")
}

// startWorker creates the worker run for an attempt. The attempt is linked
// to the run before the run executes, so its first submission finds it.
func (s *Service) startWorker(ctx context.Context, job *agentcore.WorkJob, attempt agentcore.WorkAttempt, items []agentcore.WorkItem) (*agentcore.AgentRun, error) {
	worker, err := s.worker(ctx, job.AppID, job.ID)
	if err != nil {
		return nil, err
	}
	dispatchKey := "work-job:" + attempt.ID
	if worker.ModelCredential != nil && s.cfg.Credentials != nil {
		provider := ""
		if worker.Model != nil {
			provider = worker.Model.Provider
		}
		if provider == "" {
			if agent, err := s.store.GetAgent(ctx, job.AppID, worker.AgentID); err == nil && agent != nil {
				provider = agent.Provider
			}
		}
		fresh, changed, err := s.cfg.Credentials.FreshCredential(ctx, job.AppID, provider, dispatchKey, *worker.ModelCredential, credentialMinValidity)
		if err != nil {
			return nil, err
		}
		if changed {
			worker.ModelCredential = &fresh
			if sealed, err := sealWorker(s.cfg.Credentials.Key, job.AppID, job.ID, worker); err == nil {
				_ = s.store.SetWorkJobWorker(ctx, job.AppID, job.ID, sealed)
			}
		}
	}
	instructions, err := workerInstructions(job, items)
	if err != nil {
		return nil, err
	}
	metadata := map[string]interface{}{}
	for key, value := range worker.Metadata {
		metadata[key] = value
	}
	metadata["work_job_id"], metadata["work_attempt_id"] = job.ID, attempt.ID
	var servers []mcp.RunServerRequest
	if len(worker.MCPServers) > 0 {
		raw, err := json.Marshal(worker.MCPServers)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, err
		}
	}
	req := engine.StartRunRequest{
		AppID: job.AppID, HostRunID: dispatchKey, AgentID: worker.AgentID, Target: worker.Target,
		Model: worker.Model, ModelCredential: worker.ModelCredential, AllowedTools: worker.AllowedTools,
		ExternalActorID: worker.ExternalActorID, Metadata: metadata, MCPServers: servers, Instructions: instructions,
		Mode: agentcore.InvocationAutonomous, ExecutionMode: engine.ExecutionModeLightweight,
		TurnPolicy: agentcore.TurnPolicy{CompletionMode: agentcore.TurnCompletionExplicit, MaxCompletionCorrections: 2},
		GroupID:    job.GroupID, ParentRunID: job.ParentRunID,
	}
	return s.cfg.Engine.StartWorkJobRun(ctx, req, func(run *agentcore.AgentRun) error {
		// New members start at execution generation 0.
		return s.store.SetWorkAttemptRun(ctx, job.AppID, attempt.ID, run.ID, 0)
	})
}

func (s *Service) worker(ctx context.Context, appID, jobID string) (agentcore.WorkJobWorker, error) {
	sealed, err := s.store.GetWorkJobWorker(ctx, appID, jobID)
	if err != nil {
		return agentcore.WorkJobWorker{}, err
	}
	var key []byte
	if s.cfg.Credentials != nil {
		key = s.cfg.Credentials.Key
	}
	return openWorker(key, appID, jobID, sealed)
}

func (s *Service) usesBrowser(ctx context.Context, appID string, worker agentcore.WorkJobWorker) bool {
	agent, err := s.store.GetAgent(ctx, appID, worker.AgentID)
	if err != nil || agent == nil {
		return false
	}
	for name := range tools.AllowedSet(agent, worker.AllowedTools) {
		if strings.HasPrefix(name, "browser_") {
			return true
		}
	}
	return false
}

// workerUsage reads a worker's cumulative usage: the larger of its run
// summary and its persisted native checkpoint, both cumulative counters.
func (s *Service) workerUsage(ctx context.Context, appID, runID string) (int64, int64, int64) {
	var input, output, cached int64
	read := func(raw json.RawMessage) {
		var usage struct {
			InputTokens       int64 `json:"input_tokens"`
			OutputTokens      int64 `json:"output_tokens"`
			CachedInputTokens int64 `json:"cached_input_tokens"`
		}
		if len(raw) > 0 && json.Unmarshal(raw, &usage) == nil {
			input, output, cached = max(input, usage.InputTokens), max(output, usage.OutputTokens), max(cached, usage.CachedInputTokens)
		}
	}
	if run, err := s.store.GetRun(ctx, appID, runID); err == nil && run != nil {
		read(run.OutputSummary)
	}
	if checkpoint, err := runtime.NativeCheckpointUsage(ctx, s.store, appID, runID); err == nil {
		read(checkpoint)
	}
	return input, output, cached
}

func (s *Service) applyUsage(ctx context.Context, appID string, attempt agentcore.WorkAttempt) {
	if attempt.RunID == "" {
		return
	}
	input, output, cached := s.workerUsage(ctx, appID, attempt.RunID)
	if err := s.store.ApplyWorkAttemptUsage(ctx, agentcore.WorkAttemptClose{AppID: appID, AttemptID: attempt.ID, InputTokens: input, OutputTokens: output, CachedTokens: cached, Now: s.now()}); err != nil {
		slog.WarnContext(ctx, "apply worker usage failed", "attempt_id", attempt.ID, "error", err)
	}
}

func (s *Service) closeAttempt(ctx context.Context, appID string, attempt agentcore.WorkAttempt, state, outcome string) {
	close := agentcore.WorkAttemptClose{AppID: appID, AttemptID: attempt.ID, State: state, Outcome: outcome, Now: s.now()}
	if attempt.RunID != "" {
		close.InputTokens, close.OutputTokens, close.CachedTokens = s.workerUsage(ctx, appID, attempt.RunID)
	}
	if _, err := s.store.CloseWorkAttempt(ctx, close); err != nil {
		slog.ErrorContext(ctx, "close work attempt failed", "attempt_id", attempt.ID, "error", err)
	}
}

func (s *Service) setState(ctx context.Context, job *agentcore.WorkJob, state, reason, blockedRunID string) error {
	updated, err := s.store.UpdateWorkJob(ctx, job.AppID, job.ID, agentcore.WorkJobUpdate{State: &state, WaitingReason: &reason, BlockedRunID: &blockedRunID, Now: s.now()})
	if err == nil {
		*job = *updated
	}
	return err
}

func (s *Service) fail(ctx context.Context, job *agentcore.WorkJob, message string) error {
	failed, empty := agentcore.WorkJobStateFailed, ""
	_, err := s.store.UpdateWorkJob(ctx, job.AppID, job.ID, agentcore.WorkJobUpdate{State: &failed, WaitingReason: &empty, Error: &message, Now: s.now()})
	return err
}

// EventSink wakes the controller when a worker's run changes state, so
// continuation does not wait for the next tick.
type EventSink struct{ Service *Service }

func (w EventSink) Emit(_ context.Context, event engine.Event) {
	switch event.Type {
	case "run.completed", "run.failed", "run.cancelled", "run.paused", "run.resumed":
		if w.Service != nil && strings.HasPrefix(event.HostRunID, "work-job:") {
			w.Service.Wake()
		}
	}
}
