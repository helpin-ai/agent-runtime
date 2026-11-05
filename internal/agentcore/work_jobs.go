package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/helpin-ai/agent-runtime-go"
)

type (
	WorkJob           = sdk.WorkJob
	WorkJobSpec       = sdk.WorkJobSpec
	WorkJobBudget     = sdk.WorkJobBudget
	WorkJobCounts     = sdk.WorkJobCounts
	WorkJobUsage      = sdk.WorkJobUsage
	WorkJobWorker     = sdk.WorkJobWorker
	WorkItem          = sdk.WorkItem
	WorkItemInput     = sdk.WorkItemInput
	WorkItemPage      = sdk.WorkItemPage
	WorkItemException = sdk.WorkItemException
	WorkAttempt       = sdk.WorkAttempt
	WorkJobEvent      = sdk.WorkJobEvent
	WorkJobEventPage  = sdk.WorkJobEventPage
	WorkJobDataset    = sdk.WorkJobDataset
	WorkDatasetItem   = sdk.WorkDatasetItem

	CreateWorkJobRequest = sdk.CreateWorkJobRequest
	AddWorkItemsRequest  = sdk.AddWorkItemsRequest
	ReviseWorkJobRequest = sdk.ReviseWorkJobRequest
)

const (
	WorkJobStateQueued    = sdk.WorkJobStateQueued
	WorkJobStateRunning   = sdk.WorkJobStateRunning
	WorkJobStateWaiting   = sdk.WorkJobStateWaiting
	WorkJobStatePaused    = sdk.WorkJobStatePaused
	WorkJobStateVerifying = sdk.WorkJobStateVerifying
	WorkJobStateCompleted = sdk.WorkJobStateCompleted
	WorkJobStateFailed    = sdk.WorkJobStateFailed
	WorkJobStateCancelled = sdk.WorkJobStateCancelled

	WorkJobWaitingBudget       = sdk.WorkJobWaitingBudget
	WorkJobWaitingWorkerInput  = sdk.WorkJobWaitingWorkerInput
	WorkJobWaitingWorkerAuth   = sdk.WorkJobWaitingWorkerAuth
	WorkJobWaitingNoProgress   = sdk.WorkJobWaitingNoProgress
	WorkJobWaitingExceptions   = sdk.WorkJobWaitingExceptions
	WorkJobWaitingRetryBackoff = sdk.WorkJobWaitingRetryBackoff
	WorkJobWaitingDependencies = sdk.WorkJobWaitingDependencies
	WorkJobWaitingUnavailable  = sdk.WorkJobWaitingUnavailable

	WorkJobWaitingUncertainEffects = sdk.WorkJobWaitingUncertainEffects

	WorkItemStatePending   = sdk.WorkItemStatePending
	WorkItemStateRunning   = sdk.WorkItemStateRunning
	WorkItemStateRetryWait = sdk.WorkItemStateRetryWait
	WorkItemStateBlocked   = sdk.WorkItemStateBlocked
	WorkItemStateCompleted = sdk.WorkItemStateCompleted
	WorkItemStateFailed    = sdk.WorkItemStateFailed
	WorkItemStateCancelled = sdk.WorkItemStateCancelled

	WorkItemOutcomeCompleted = sdk.WorkItemOutcomeCompleted
	WorkItemOutcomeNoResult  = sdk.WorkItemOutcomeNoResult
	WorkItemOutcomeException = sdk.WorkItemOutcomeException

	WorkAttemptStateOpen      = sdk.WorkAttemptStateOpen
	WorkAttemptStateCompleted = sdk.WorkAttemptStateCompleted
	WorkAttemptStatePartial   = sdk.WorkAttemptStatePartial
	WorkAttemptStateFailed    = sdk.WorkAttemptStateFailed
	WorkAttemptStateCancelled = sdk.WorkAttemptStateCancelled
)

var (
	ErrWorkJobsUnsupported = errors.New("work jobs are not supported by this store")
	ErrWorkJobNotFound     = errors.New("work job not found")
	// ErrWorkJobRevisionConflict rejects a revision based on a stale spec.
	ErrWorkJobRevisionConflict = errors.New("work job specification changed; reload and retry")
)

// IsTerminalWorkJobState reports whether a job can change no further.
func IsTerminalWorkJobState(state string) bool {
	switch state {
	case WorkJobStateCompleted, WorkJobStateFailed, WorkJobStateCancelled:
		return true
	}
	return false
}

// NewWorkJob is a job about to be created. SealedWorker is the encrypted
// worker template; the store never decrypts it.
type NewWorkJob struct {
	AppID          string
	ID             string
	GroupID        string
	IdempotencyKey string
	ParentRunID    string
	Spec           WorkJobSpec
	SealedWorker   []byte
	Items          []WorkItemInput
}

// WorkClaim requests one batch for a new attempt.
type WorkClaim struct {
	AppID       string
	JobID       string
	AttemptID   string
	BatchSize   int
	Reserve     int64
	UsesBrowser bool
	Now         time.Time
}

// WorkResult is one submit_item_result call after deterministic validation.
// AppID, AttemptID and RunID come from the trusted call context, never from
// the model.
type WorkResult struct {
	AppID     string
	AttemptID string
	RunID     string
	ItemID    string
	Outcome   string
	Result    json.RawMessage
	Exception *WorkItemException
	// Digest identifies the submitted content; an identical replay from the
	// same attempt returns the original receipt.
	Digest string
	// Retryable schedules the item for a fresh attempt instead of accepting
	// the exception, unless the item's attempts are exhausted.
	Retryable  bool
	RetryAfter time.Duration
	Now        time.Time
}

// Receipt statuses returned to the worker.
const (
	WorkReceiptAccepted        = "accepted"
	WorkReceiptAlreadyAccepted = "already_accepted"
	WorkReceiptRetryScheduled  = "retry_scheduled"
)

type WorkReceipt struct {
	ItemID       string `json:"item_id"`
	Status       string `json:"status"`
	Outcome      string `json:"outcome"`
	SpecRevision int    `json:"spec_revision"`
	Remaining    int    `json:"remaining_in_batch"`
}

// WorkRejectedError explains why a result was not accepted. The worker may
// correct and resubmit.
type WorkRejectedError struct{ Reason string }

func (e *WorkRejectedError) Error() string { return e.Reason }

// WorkAttemptClose ends an attempt. Usage is the worker run's cumulative
// persisted usage; only the part above the attempt's watermark is applied.
type WorkAttemptClose struct {
	AppID        string
	AttemptID    string
	State        string
	Outcome      string
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
	// BlockUnresolved holds unresolved items as blocked instead of retrying
	// them, because the worker made changes whose outcome is unknown.
	BlockUnresolved bool
	Now             time.Time
}

// WorkJobUpdate changes a job's lifecycle fields. Nil fields are unchanged.
type WorkJobUpdate struct {
	State         *string
	WaitingReason *string
	BlockedRunID  *string
	Error         *string
	// ResetNoProgress clears the consecutive no-progress counter after a
	// user decision.
	ResetNoProgress bool
	// AcceptExceptions records that the user accepted the current exception
	// count, so verification does not block on it again.
	AcceptExceptions bool
	Now              time.Time
}

// WorkJobStore persists work jobs in the main store. Implementations are
// SQLite (phase 1a) and, once migrated, Postgres; other stores do not
// implement it and jobs are unavailable on them.
type WorkJobStore interface {
	WorkJobsSupported() bool
	// CreateWorkJob creates the job, its work-job run group and its items in
	// one transaction. An existing job with the same idempotency key is
	// returned with created=false.
	CreateWorkJob(ctx context.Context, job NewWorkJob) (*WorkJob, bool, error)
	GetWorkJob(ctx context.Context, appID, jobID string) (*WorkJob, error)
	ListWorkJobs(ctx context.Context, appID string) ([]WorkJob, error)
	// ListActiveWorkJobs returns non-terminal jobs of every app.
	ListActiveWorkJobs(ctx context.Context) ([]WorkJob, error)
	GetWorkJobWorker(ctx context.Context, appID, jobID string) ([]byte, error)
	SetWorkJobWorker(ctx context.Context, appID, jobID string, sealed []byte) error
	UpdateWorkJob(ctx context.Context, appID, jobID string, update WorkJobUpdate) (*WorkJob, error)
	// WorkJobProgress returns the consecutive attempts that accepted nothing
	// and the exception count the user last accepted (-1 when never).
	WorkJobProgress(ctx context.Context, appID, jobID string) (noProgress, acceptedExceptions int, err error)
	AddWorkItems(ctx context.Context, appID, jobID string, items []WorkItemInput) (*WorkJob, error)
	ReviseWorkJob(ctx context.Context, appID, jobID string, expectedRevision int, spec WorkJobSpec, now time.Time) (*WorkJob, error)

	ListWorkItems(ctx context.Context, appID, jobID, state, after string, limit int) (*WorkItemPage, error)
	GetWorkItems(ctx context.Context, appID, jobID string, itemIDs []string) ([]WorkItem, error)
	// CancelWorkItems marks every item without an accepted result cancelled.
	CancelWorkItems(ctx context.Context, appID, jobID string, now time.Time) error
	// UnblockWorkItems returns blocked items to pending after a user decision.
	UnblockWorkItems(ctx context.Context, appID, jobID string, now time.Time) (int, error)
	// FailBlockedWorkItems fails pending items whose prerequisite can no
	// longer succeed, and returns how many were failed.
	FailBlockedWorkItems(ctx context.Context, appID, jobID string, now time.Time) (int, error)
	// RequeueWorkItems returns completed items to pending, for revalidation
	// after a spec revision.
	RequeueWorkItems(ctx context.Context, appID, jobID string, itemIDs []string, reason string, now time.Time) error

	// ClaimWorkBatch atomically checks the job's state and budget, assigns
	// up to BatchSize eligible items to a new open attempt and reserves its
	// budget. It returns nil when nothing is eligible.
	ClaimWorkBatch(ctx context.Context, claim WorkClaim) (*WorkAttempt, []WorkItem, error)
	// SetWorkAttemptRun links an attempt to its worker run and records the
	// execution generation that may submit results for it.
	SetWorkAttemptRun(ctx context.Context, appID, attemptID, runID string, generation int64) error
	GetWorkAttempt(ctx context.Context, appID, attemptID string) (*WorkAttempt, error)
	GetWorkAttemptByRun(ctx context.Context, appID, runID string) (*WorkAttempt, error)
	ListWorkAttempts(ctx context.Context, appID, jobID string, openOnly bool) ([]WorkAttempt, error)
	// AcceptWorkResult fences on the attempt's run and generation and on the
	// item's assignment, then records the result. Rejections are returned as
	// *WorkRejectedError.
	AcceptWorkResult(ctx context.Context, result WorkResult) (*WorkReceipt, error)
	// ApplyWorkAttemptUsage applies usage of a still-running attempt.
	ApplyWorkAttemptUsage(ctx context.Context, close WorkAttemptClose) error
	// CloseWorkAttempt applies final usage, releases the reservation and
	// returns unresolved items to retry or failure.
	CloseWorkAttempt(ctx context.Context, close WorkAttemptClose) (*WorkAttempt, error)

	AppendWorkJobEvent(ctx context.Context, appID, jobID, eventType string, data map[string]interface{}) error
	ListWorkJobEvents(ctx context.Context, appID, jobID string, after int64, limit int) (*WorkJobEventPage, error)

	// PublishWorkJobDataset writes a new dataset revision from accepted
	// items. An unchanged dataset keeps its revision.
	PublishWorkJobDataset(ctx context.Context, appID, jobID string, now time.Time) (*WorkJobDataset, error)
	GetWorkJobDataset(ctx context.Context, appID, jobID string) (*WorkJobDataset, error)
}
