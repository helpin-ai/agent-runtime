package workjobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/id"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Unavailability reasons reported through capabilities.
const (
	ReasonUnsupportedStore = "unsupported_store"
	ReasonAppDisabled      = "app_disabled"
)

var (
	ErrUnsupported = errors.New("work jobs are not supported by this runtime store")
	ErrDisabled    = errors.New("work jobs are not enabled for this app")
)

// Store is the main store with run-group and work-job support.
type Store interface {
	agentcore.Store
	agentcore.RunGroupStore
	agentcore.WorkJobStore
}

type Config struct {
	Engine *engine.Engine
	Store  agentcore.Store
	Tools  *tools.Registry
	// Credentials seals worker templates and refreshes their model
	// credential before a worker starts.
	Credentials *modelauth.Manager
	// Enabled reports whether an app opted into work jobs.
	Enabled func(appID string) bool
	// Interval is the reconcile tick; wake-ups make it a fallback.
	Interval time.Duration
	// RetryBackoff delays a retryable exception's next attempt.
	RetryBackoff time.Duration
	Now          func() time.Time
}

type Service struct {
	cfg   Config
	store Store
	wake  chan struct{}
	// mu serializes controller steps with user actions on jobs.
	mu sync.Mutex
}

// New returns a service, or one whose Available reports unsupported_store
// when the store has no work-job tables. It registers submit_item_result.
func New(cfg Config) *Service {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = defaultRetryBackoff
	}
	s := &Service{cfg: cfg, wake: make(chan struct{}, 1)}
	if jobs, ok := cfg.Store.(Store); ok && jobs.WorkJobsSupported() && jobs.RunGroupsSupported() {
		s.store = jobs
	}
	if cfg.Tools != nil && s.store != nil {
		cfg.Tools.Register(submitToolDefinition(), s.handleSubmit)
	}
	return s
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// Wake asks the controller to reconcile now.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Available reports whether appID may use work jobs, and why not.
func (s *Service) Available(appID string) (bool, string) {
	if s == nil || s.store == nil {
		return false, ReasonUnsupportedStore
	}
	if s.cfg.Enabled == nil || !s.cfg.Enabled(strings.TrimSpace(appID)) {
		return false, ReasonAppDisabled
	}
	return true, ""
}

func (s *Service) require(appID string) error {
	if ok, reason := s.Available(appID); !ok {
		if reason == ReasonUnsupportedStore {
			return ErrUnsupported
		}
		return ErrDisabled
	}
	return nil
}

// ValidationError rejects a request; its message is safe to return.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

func (s *Service) Create(ctx context.Context, req agentcore.CreateWorkJobRequest) (*agentcore.WorkJob, error) {
	req.AppID = strings.TrimSpace(req.AppID)
	if err := s.require(req.AppID); err != nil {
		return nil, err
	}
	spec, err := normalizeSpec(req.Spec)
	if err != nil {
		return nil, invalid("%v", err)
	}
	worker, err := normalizeWorker(req.Worker)
	if err != nil {
		return nil, invalid("%v", err)
	}
	if err := validateItems(req.Items, 0); err != nil {
		return nil, invalid("%v", err)
	}
	if err := checkDependencies(req.Items, func(string) bool { return false }); err != nil {
		return nil, invalid("%v", err)
	}
	agent, err := s.store.GetAgent(ctx, req.AppID, worker.AgentID)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, invalid("worker agent not found")
	}
	if err := tools.ValidateAllowedSubset(agent, worker.AllowedTools); err != nil {
		return nil, invalid("%v", err)
	}
	if worker.ModelCredential != nil {
		provider := agent.Provider
		if worker.Model != nil {
			provider = worker.Model.Provider
		}
		if err := modelauth.Validate(provider, *worker.ModelCredential); err != nil {
			return nil, invalid("worker.model_credential: %v", err)
		}
	}
	if parent := strings.TrimSpace(req.ParentRunID); parent != "" {
		if run, err := s.store.GetRun(ctx, req.AppID, parent); err != nil || run == nil {
			return nil, invalid("parent run not found")
		}
	}
	jobID := id.New("workjob")
	var key []byte
	if s.cfg.Credentials != nil {
		key = s.cfg.Credentials.Key
	}
	sealed, err := sealWorker(key, req.AppID, jobID, worker)
	if err != nil {
		return nil, invalid("%v", err)
	}
	job, _, err := s.store.CreateWorkJob(ctx, agentcore.NewWorkJob{
		AppID: req.AppID, ID: jobID, GroupID: id.New("rungroup"), IdempotencyKey: req.IdempotencyKey,
		ParentRunID: strings.TrimSpace(req.ParentRunID), Spec: spec, SealedWorker: sealed, Items: req.Items,
	})
	if err != nil {
		return nil, err
	}
	s.Wake()
	return job, nil
}

func (s *Service) Get(ctx context.Context, appID, jobID string) (*agentcore.WorkJob, error) {
	if err := s.require(appID); err != nil {
		return nil, err
	}
	return s.store.GetWorkJob(ctx, appID, jobID)
}

func (s *Service) List(ctx context.Context, appID string) ([]agentcore.WorkJob, error) {
	if err := s.require(appID); err != nil {
		return nil, err
	}
	return s.store.ListWorkJobs(ctx, appID)
}

func (s *Service) Items(ctx context.Context, appID, jobID, state, after string, limit int) (*agentcore.WorkItemPage, error) {
	if _, err := s.Get(ctx, appID, jobID); err != nil {
		return nil, err
	}
	return s.store.ListWorkItems(ctx, appID, jobID, state, after, limit)
}

func (s *Service) Attempts(ctx context.Context, appID, jobID string) ([]agentcore.WorkAttempt, error) {
	if _, err := s.Get(ctx, appID, jobID); err != nil {
		return nil, err
	}
	return s.store.ListWorkAttempts(ctx, appID, jobID, false)
}

func (s *Service) Events(ctx context.Context, appID, jobID string, after int64, limit int) (*agentcore.WorkJobEventPage, error) {
	if _, err := s.Get(ctx, appID, jobID); err != nil {
		return nil, err
	}
	return s.store.ListWorkJobEvents(ctx, appID, jobID, after, limit)
}

func (s *Service) Dataset(ctx context.Context, appID, jobID string) (*agentcore.WorkJobDataset, error) {
	if _, err := s.Get(ctx, appID, jobID); err != nil {
		return nil, err
	}
	dataset, err := s.store.GetWorkJobDataset(ctx, appID, jobID)
	if err != nil || dataset != nil {
		return dataset, err
	}
	// Before the first publication the dataset is empty, not missing.
	return &agentcore.WorkJobDataset{JobID: jobID, Items: []agentcore.WorkDatasetItem{}}, nil
}

func (s *Service) AddItems(ctx context.Context, appID, jobID string, req agentcore.AddWorkItemsRequest) (*agentcore.WorkJob, error) {
	job, err := s.Get(ctx, appID, jobID)
	if err != nil {
		return nil, err
	}
	if err := validateItems(req.Items, job.Counts.Total); err != nil {
		return nil, invalid("%v", err)
	}
	if len(req.Items) == 0 {
		return job, nil
	}
	existing := map[string]bool{}
	for after := ""; ; {
		page, err := s.store.ListWorkItems(ctx, appID, jobID, "", after, 1000)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			existing[item.ID] = true
		}
		if page.NextAfter == "" {
			break
		}
		after = page.NextAfter
	}
	if err := checkDependencies(req.Items, func(id string) bool { return existing[id] }); err != nil {
		return nil, invalid("%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, err = s.store.AddWorkItems(ctx, appID, jobID, req.Items)
	var rejected *agentcore.WorkRejectedError
	if errors.As(err, &rejected) {
		return nil, invalid("%s", rejected.Reason)
	}
	s.Wake()
	return job, err
}

// Pause stops new dispatch. Running workers finish their batch.
func (s *Service) Pause(ctx context.Context, appID, jobID string) (*agentcore.WorkJob, error) {
	job, err := s.Get(ctx, appID, jobID)
	if err != nil {
		return nil, err
	}
	if agentcore.IsTerminalWorkJobState(job.State) {
		return job, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	paused := agentcore.WorkJobStatePaused
	return s.store.UpdateWorkJob(ctx, appID, jobID, agentcore.WorkJobUpdate{State: &paused, Now: s.now()})
}

// Resume continues a paused job, or one waiting on a user decision: no
// progress (the counter restarts), exceptions over policy (the current count
// is accepted) or budget (admission is rechecked against the current spec).
func (s *Service) Resume(ctx context.Context, appID, jobID string) (*agentcore.WorkJob, error) {
	job, err := s.Get(ctx, appID, jobID)
	if err != nil {
		return nil, err
	}
	update := agentcore.WorkJobUpdate{Now: s.now()}
	switch {
	case job.State == agentcore.WorkJobStatePaused:
	case job.State == agentcore.WorkJobStateWaiting && job.WaitingReason == agentcore.WorkJobWaitingNoProgress:
		update.ResetNoProgress = true
	case job.State == agentcore.WorkJobStateWaiting && job.WaitingReason == agentcore.WorkJobWaitingExceptions:
		update.AcceptExceptions = true
	case job.State == agentcore.WorkJobStateWaiting && job.WaitingReason == agentcore.WorkJobWaitingBudget:
	default:
		// Worker input, approval and authentication are answered on the
		// worker run; other states need no resume.
		return job, nil
	}
	running, empty := agentcore.WorkJobStateRunning, ""
	update.State, update.WaitingReason, update.BlockedRunID = &running, &empty, &empty
	s.mu.Lock()
	job, err = s.store.UpdateWorkJob(ctx, appID, jobID, update)
	s.mu.Unlock()
	s.Wake()
	return job, err
}

// Cancel stops dispatch permanently and cancels active workers. Accepted
// results and the published dataset are kept.
func (s *Service) Cancel(ctx context.Context, appID, jobID string) (*agentcore.WorkJob, error) {
	job, err := s.Get(ctx, appID, jobID)
	if err != nil {
		return nil, err
	}
	if agentcore.IsTerminalWorkJobState(job.State) {
		return job, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Closing the group first stops any further worker admission.
	cancelErr := s.cfg.Engine.CancelWorkJobRuns(ctx, appID, job.GroupID)
	attempts, err := s.store.ListWorkAttempts(ctx, appID, jobID, true)
	if err != nil {
		return nil, err
	}
	for _, attempt := range attempts {
		s.closeAttempt(ctx, appID, attempt, agentcore.WorkAttemptStateCancelled, "job cancelled")
	}
	if err := s.store.CancelWorkItems(ctx, appID, jobID, s.now()); err != nil {
		return nil, err
	}
	if _, err := s.store.PublishWorkJobDataset(ctx, appID, jobID, s.now()); err != nil {
		return nil, err
	}
	cancelled, empty := agentcore.WorkJobStateCancelled, ""
	job, err = s.store.UpdateWorkJob(ctx, appID, jobID, agentcore.WorkJobUpdate{State: &cancelled, WaitingReason: &empty, BlockedRunID: &empty, Now: s.now()})
	if err != nil {
		return nil, err
	}
	return job, cancelErr
}

// Revise replaces the specification at a safe boundary: running workers keep
// the revision they were given and their results record it; new attempts use
// the new one. With revalidate, accepted results that fail the new contract
// are requeued.
func (s *Service) Revise(ctx context.Context, appID, jobID string, req agentcore.ReviseWorkJobRequest) (*agentcore.WorkJob, error) {
	if _, err := s.Get(ctx, appID, jobID); err != nil {
		return nil, err
	}
	spec, err := normalizeSpec(req.Spec)
	if err != nil {
		return nil, invalid("%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, err := s.store.ReviseWorkJob(ctx, appID, jobID, req.ExpectedRevision, spec, s.now())
	var rejected *agentcore.WorkRejectedError
	if errors.As(err, &rejected) {
		return nil, invalid("%s", rejected.Reason)
	}
	if err != nil {
		return nil, err
	}
	if req.Revalidate {
		var stale []string
		for after := ""; ; {
			page, err := s.store.ListWorkItems(ctx, appID, jobID, agentcore.WorkItemStateCompleted, after, 1000)
			if err != nil {
				return nil, err
			}
			for _, item := range page.Items {
				input := submitInput{ItemID: item.ID, Outcome: item.Outcome, Result: item.Result, Exception: item.Exception}
				if len(validateSubmission(spec, &input)) > 0 {
					stale = append(stale, item.ID)
				}
			}
			if page.NextAfter == "" {
				break
			}
			after = page.NextAfter
		}
		if err := s.store.RequeueWorkItems(ctx, appID, jobID, stale, fmt.Sprintf("result no longer satisfies spec revision %d", job.SpecRevision), s.now()); err != nil {
			return nil, err
		}
		if job, err = s.store.GetWorkJob(ctx, appID, jobID); err != nil {
			return nil, err
		}
	}
	s.Wake()
	return job, nil
}
