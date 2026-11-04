package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// Work job tables live in the main store next to agent_runs. They are
// migrated only for SQLite in phase 1a; stores without them report jobs
// unsupported and never query them. Every write transaction updates the job
// row first, so SQLite takes its write lock before any read and concurrent
// transitions of one job serialize on that row.

type workJobRecord struct {
	AppID          string    `gorm:"primaryKey;uniqueIndex:idx_work_jobs_creation,priority:1;index:idx_work_jobs_state,priority:2"`
	ID             string    `gorm:"primaryKey"`
	IdempotencyKey *string   `gorm:"uniqueIndex:idx_work_jobs_creation,priority:2"`
	GroupID        string    `gorm:"not null"`
	ParentRunID    string    `gorm:"not null;default:''"`
	State          string    `gorm:"not null;index:idx_work_jobs_state,priority:1"`
	WaitingReason  string    `gorm:"not null;default:''"`
	BlockedRunID   string    `gorm:"not null;default:''"`
	Error          string    `gorm:"not null;default:''"`
	SpecRevision   int       `gorm:"not null;default:1"`
	Spec           jsonBytes `gorm:"type:json"`
	SealedWorker   []byte
	TotalItems     int   `gorm:"not null;default:0"`
	InputTokens    int64 `gorm:"not null;default:0"`
	OutputTokens   int64 `gorm:"not null;default:0"`
	CachedTokens   int64 `gorm:"not null;default:0"`
	ReservedTokens int64 `gorm:"not null;default:0"`
	NoProgress     int   `gorm:"not null;default:0"`
	// AcceptedExceptions is the exception count the user accepted; the
	// exception policy blocks again only above it.
	AcceptedExceptions int   `gorm:"not null;default:-1"`
	LastEventSeq       int64 `gorm:"not null;default:0"`
	DatasetRevision    int   `gorm:"not null;default:0"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CompletedAt        *time.Time
}

func (workJobRecord) TableName() string { return "agent_work_jobs" }

type workItemRecord struct {
	AppID           string    `gorm:"primaryKey;index:idx_work_items_claim,priority:1"`
	JobID           string    `gorm:"primaryKey;index:idx_work_items_claim,priority:2"`
	ID              string    `gorm:"primaryKey"`
	Seq             int       `gorm:"not null;index:idx_work_items_claim,priority:4"`
	State           string    `gorm:"not null;index:idx_work_items_claim,priority:3"`
	Input           jsonBytes `gorm:"type:json"`
	DependsOn       string    `gorm:"type:json;not null;default:'[]'"`
	Attempts        int       `gorm:"not null;default:0"`
	Outcome         string    `gorm:"not null;default:''"`
	Result          []byte    `gorm:"type:json"`
	ExceptionKind   string    `gorm:"not null;default:''"`
	ExceptionDetail string    `gorm:"not null;default:''"`
	ResultDigest    string    `gorm:"not null;default:''"`
	SpecRevision    int       `gorm:"not null;default:0"`
	AttemptID       string    `gorm:"not null;default:''"`
	RunID           string    `gorm:"not null;default:''"`
	LastError       string    `gorm:"not null;default:''"`
	RetryAt         *time.Time
	UpdatedAt       time.Time
}

func (workItemRecord) TableName() string { return "agent_work_items" }

type workAttemptRecord struct {
	AppID      string `gorm:"primaryKey;index:idx_work_attempts_job,priority:1;index:idx_work_attempts_run,priority:1"`
	ID         string `gorm:"primaryKey"`
	JobID      string `gorm:"not null;index:idx_work_attempts_job,priority:2"`
	RunID      string `gorm:"not null;default:'';index:idx_work_attempts_run,priority:2"`
	State      string `gorm:"not null;index:idx_work_attempts_job,priority:3"`
	ItemIDs    string `gorm:"type:json;not null"`
	Accepted   int    `gorm:"not null;default:0"`
	Generation int64  `gorm:"not null;default:0"`
	Outcome    string `gorm:"not null;default:''"`
	// UsageInput/Output/Cached are the worker run's cumulative counters
	// already applied to the job: the per-attempt watermark.
	UsageInput  int64 `gorm:"not null;default:0"`
	UsageOutput int64 `gorm:"not null;default:0"`
	UsageCached int64 `gorm:"not null;default:0"`
	Reserved    int64 `gorm:"not null;default:0"`
	UsesBrowser bool  `gorm:"not null;default:false"`
	StartedAt   time.Time
	EndedAt     *time.Time
}

func (workAttemptRecord) TableName() string { return "agent_work_attempts" }

type workJobEventRecord struct {
	AppID     string    `gorm:"primaryKey"`
	JobID     string    `gorm:"primaryKey"`
	Seq       int64     `gorm:"primaryKey"`
	Type      string    `gorm:"not null"`
	Data      jsonBytes `gorm:"type:json"`
	CreatedAt time.Time
}

func (workJobEventRecord) TableName() string { return "agent_work_job_events" }

type workDatasetRecord struct {
	AppID     string `gorm:"primaryKey"`
	JobID     string `gorm:"primaryKey"`
	Revision  int    `gorm:"primaryKey"`
	Digest    string `gorm:"not null"`
	Items     []byte `gorm:"type:json"`
	CreatedAt time.Time
}

func (workDatasetRecord) TableName() string { return "agent_work_datasets" }

const (
	defaultWorkItemPage = 100
	maxWorkItemPage     = 1000
	maxWorkJobEvents    = 500
	workTxRetries       = 6
)

func (s *SQL) WorkJobsSupported() bool { return s != nil && s.workJobs }

func (s *SQL) migrateWorkJobs() error {
	if err := s.db.AutoMigrate(&workJobRecord{}, &workItemRecord{}, &workAttemptRecord{}, &workJobEventRecord{}, &workDatasetRecord{}); err != nil {
		return fmt.Errorf("migrate work jobs: %w", err)
	}
	s.workJobs = true
	return nil
}

// workTx runs fn in a transaction, retrying bounded times when SQLite
// reports the database busy.
func (s *SQL) workTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	if !s.WorkJobsSupported() {
		return agentcore.ErrWorkJobsUnsupported
	}
	var err error
	for attempt := 0; attempt < workTxRetries; attempt++ {
		err = s.db.WithContext(ctx).Transaction(fn)
		if err == nil || !sqliteBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(25*(attempt+1)) * time.Millisecond):
		}
	}
	return err
}

func sqliteBusy(err error) bool {
	text := err.Error()
	return strings.Contains(text, "database is locked") || strings.Contains(text, "SQLITE_BUSY") || strings.Contains(text, "database table is locked")
}

// lockWorkJob touches the job row so the transaction holds the write lock,
// then loads it.
func lockWorkJob(tx *gorm.DB, appID, jobID string, now time.Time) (*workJobRecord, error) {
	result := tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", appID, jobID).UpdateColumn("updated_at", now)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, agentcore.ErrWorkJobNotFound
	}
	var job workJobRecord
	if err := tx.Where("app_id = ? AND id = ?", appID, jobID).Take(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func appendWorkEvent(tx *gorm.DB, job *workJobRecord, eventType string, data map[string]interface{}, now time.Time) error {
	job.LastEventSeq++
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := tx.Create(&workJobEventRecord{AppID: job.AppID, JobID: job.ID, Seq: job.LastEventSeq, Type: eventType, Data: raw, CreatedAt: now}).Error; err != nil {
		return err
	}
	return tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", job.AppID, job.ID).UpdateColumn("last_event_seq", job.LastEventSeq).Error
}

func (s *SQL) CreateWorkJob(ctx context.Context, input agentcore.NewWorkJob) (*agentcore.WorkJob, bool, error) {
	if !s.WorkJobsSupported() || !s.RunGroupsSupported() {
		return nil, false, agentcore.ErrWorkJobsUnsupported
	}
	spec, err := json.Marshal(input.Spec)
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	record := workJobRecord{
		AppID: input.AppID, ID: input.ID, GroupID: input.GroupID, ParentRunID: input.ParentRunID,
		State: agentcore.WorkJobStateQueued, SpecRevision: 1, Spec: spec, SealedWorker: input.SealedWorker,
		TotalItems: len(input.Items), AcceptedExceptions: -1, CreatedAt: now, UpdatedAt: now,
	}
	if key := strings.TrimSpace(input.IdempotencyKey); key != "" {
		record.IdempotencyKey = &key
	}
	created := true
	err = s.workTx(ctx, func(tx *gorm.DB) error {
		if record.IdempotencyKey != nil {
			var existing workJobRecord
			err := tx.Where("app_id = ? AND idempotency_key = ?", record.AppID, *record.IdempotencyKey).Take(&existing).Error
			if err == nil {
				record, created = existing, false
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		policy, _ := json.Marshal(agentcore.RunGroupPolicy{})
		groupKey := "work_job:" + record.ID
		if err := tx.Create(&runGroupRecord{
			AppID: record.AppID, ID: record.GroupID, CreationKey: &groupKey, OwnerKind: agentcore.RunGroupOwnerWorkJob,
			ParentRunID: record.ParentRunID, State: agentcore.RunGroupStateActive, Policy: policy, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		items, err := newWorkItemRecords(record.AppID, record.ID, 0, input.Items, now)
		if err != nil {
			return err
		}
		if len(items) > 0 {
			if err := tx.CreateInBatches(items, 200).Error; err != nil {
				return err
			}
		}
		return appendWorkEvent(tx, &record, "work_job.created", map[string]interface{}{"items": len(items)}, now)
	})
	if err != nil {
		return nil, false, err
	}
	job, err := s.workJobView(ctx, s.db.WithContext(ctx), &record)
	return job, created, err
}

func newWorkItemRecords(appID, jobID string, firstSeq int, inputs []agentcore.WorkItemInput, now time.Time) ([]workItemRecord, error) {
	out := make([]workItemRecord, 0, len(inputs))
	for i, item := range inputs {
		deps, err := json.Marshal(nonNilStrings(item.DependsOn))
		if err != nil {
			return nil, err
		}
		input := item.Input
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		out = append(out, workItemRecord{
			AppID: appID, JobID: jobID, ID: item.ID, Seq: firstSeq + i + 1, State: agentcore.WorkItemStatePending,
			Input: jsonBytes(input), DependsOn: string(deps), UpdatedAt: now,
		})
	}
	return out, nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func (s *SQL) GetWorkJob(ctx context.Context, appID, jobID string) (*agentcore.WorkJob, error) {
	if !s.WorkJobsSupported() {
		return nil, agentcore.ErrWorkJobsUnsupported
	}
	var record workJobRecord
	err := s.db.WithContext(ctx).Where("app_id = ? AND id = ?", appID, jobID).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, agentcore.ErrWorkJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.workJobView(ctx, s.db.WithContext(ctx), &record)
}

func (s *SQL) ListWorkJobs(ctx context.Context, appID string) ([]agentcore.WorkJob, error) {
	return s.listWorkJobs(ctx, s.db.WithContext(ctx).Where("app_id = ?", appID))
}

func (s *SQL) ListActiveWorkJobs(ctx context.Context) ([]agentcore.WorkJob, error) {
	return s.listWorkJobs(ctx, s.db.WithContext(ctx).Where("state NOT IN ?", []string{agentcore.WorkJobStateCompleted, agentcore.WorkJobStateFailed, agentcore.WorkJobStateCancelled}))
}

func (s *SQL) listWorkJobs(ctx context.Context, query *gorm.DB) ([]agentcore.WorkJob, error) {
	if !s.WorkJobsSupported() {
		return nil, agentcore.ErrWorkJobsUnsupported
	}
	var records []workJobRecord
	if err := query.Order("created_at DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	out := make([]agentcore.WorkJob, 0, len(records))
	for i := range records {
		job, err := s.workJobView(ctx, s.db.WithContext(ctx), &records[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	return out, nil
}

func (s *SQL) workJobView(_ context.Context, db *gorm.DB, record *workJobRecord) (*agentcore.WorkJob, error) {
	job := &agentcore.WorkJob{
		ID: record.ID, AppID: record.AppID, GroupID: record.GroupID, ParentRunID: record.ParentRunID,
		State: record.State, WaitingReason: record.WaitingReason, BlockedRunID: record.BlockedRunID, Error: record.Error,
		SpecRevision: record.SpecRevision, DatasetRevision: record.DatasetRevision,
		Usage:     agentcore.WorkJobUsage{InputTokens: record.InputTokens, OutputTokens: record.OutputTokens, CachedInputTokens: record.CachedTokens, ReservedTokens: record.ReservedTokens},
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, CompletedAt: record.CompletedAt, ActiveRuns: []string{},
	}
	if err := json.Unmarshal(record.Spec, &job.Spec); err != nil {
		return nil, fmt.Errorf("decode work job spec: %w", err)
	}
	counts, err := workJobCounts(db, record.AppID, record.ID)
	if err != nil {
		return nil, err
	}
	job.Counts = counts
	var runs []string
	if err := db.Model(&workAttemptRecord{}).Where("app_id = ? AND job_id = ? AND state = ? AND run_id <> ''", record.AppID, record.ID, agentcore.WorkAttemptStateOpen).
		Order("started_at").Pluck("run_id", &runs).Error; err != nil {
		return nil, err
	}
	job.ActiveRuns = append(job.ActiveRuns, runs...)
	return job, nil
}

func workJobCounts(db *gorm.DB, appID, jobID string) (agentcore.WorkJobCounts, error) {
	var rows []struct {
		State   string
		Outcome string
		N       int
	}
	if err := db.Model(&workItemRecord{}).Select("state, outcome, count(*) AS n").Where("app_id = ? AND job_id = ?", appID, jobID).
		Group("state, outcome").Scan(&rows).Error; err != nil {
		return agentcore.WorkJobCounts{}, err
	}
	var counts agentcore.WorkJobCounts
	for _, row := range rows {
		counts.Total += row.N
		switch row.State {
		case agentcore.WorkItemStatePending:
			counts.Pending += row.N
		case agentcore.WorkItemStateRunning:
			counts.Running += row.N
		case agentcore.WorkItemStateRetryWait:
			counts.RetryWait += row.N
		case agentcore.WorkItemStateBlocked:
			counts.Blocked += row.N
		case agentcore.WorkItemStateFailed:
			counts.Failed += row.N
		case agentcore.WorkItemStateCancelled:
			counts.Cancelled += row.N
		case agentcore.WorkItemStateCompleted:
			counts.Completed += row.N
			switch row.Outcome {
			case agentcore.WorkItemOutcomeCompleted:
				counts.Found += row.N
			case agentcore.WorkItemOutcomeNoResult:
				counts.NoResult += row.N
			case agentcore.WorkItemOutcomeException:
				counts.Exceptions += row.N
			}
		}
	}
	return counts, nil
}

func (s *SQL) GetWorkJobWorker(ctx context.Context, appID, jobID string) ([]byte, error) {
	if !s.WorkJobsSupported() {
		return nil, agentcore.ErrWorkJobsUnsupported
	}
	var record workJobRecord
	err := s.db.WithContext(ctx).Select("sealed_worker").Where("app_id = ? AND id = ?", appID, jobID).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, agentcore.ErrWorkJobNotFound
	}
	return record.SealedWorker, err
}

func (s *SQL) SetWorkJobWorker(ctx context.Context, appID, jobID string, sealed []byte) error {
	return s.workTx(ctx, func(tx *gorm.DB) error {
		if _, err := lockWorkJob(tx, appID, jobID, time.Now().UTC()); err != nil {
			return err
		}
		return tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", appID, jobID).Update("sealed_worker", sealed).Error
	})
}

func (s *SQL) UpdateWorkJob(ctx context.Context, appID, jobID string, update agentcore.WorkJobUpdate) (*agentcore.WorkJob, error) {
	now := nowOr(update.Now)
	var job *workJobRecord
	err := s.workTx(ctx, func(tx *gorm.DB) error {
		var err error
		job, err = lockWorkJob(tx, appID, jobID, now)
		if err != nil {
			return err
		}
		if agentcore.IsTerminalWorkJobState(job.State) {
			return nil
		}
		changes := map[string]interface{}{}
		previous := job.State
		if update.State != nil && *update.State != job.State {
			job.State = *update.State
			changes["state"] = job.State
			if agentcore.IsTerminalWorkJobState(job.State) {
				job.CompletedAt = &now
				changes["completed_at"] = now
			}
		}
		if update.WaitingReason != nil && *update.WaitingReason != job.WaitingReason {
			job.WaitingReason = *update.WaitingReason
			changes["waiting_reason"] = job.WaitingReason
		}
		if update.BlockedRunID != nil && *update.BlockedRunID != job.BlockedRunID {
			job.BlockedRunID = *update.BlockedRunID
			changes["blocked_run_id"] = job.BlockedRunID
		}
		if update.Error != nil && *update.Error != job.Error {
			job.Error = *update.Error
			changes["error"] = job.Error
		}
		if update.ResetNoProgress && job.NoProgress != 0 {
			job.NoProgress = 0
			changes["no_progress"] = 0
		}
		if update.AcceptExceptions {
			counts, err := workJobCounts(tx, appID, jobID)
			if err != nil {
				return err
			}
			job.AcceptedExceptions = counts.Exceptions
			changes["accepted_exceptions"] = job.AcceptedExceptions
		}
		if len(changes) == 0 {
			return nil
		}
		if err := tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", appID, jobID).Updates(changes).Error; err != nil {
			return err
		}
		if job.State != previous || changes["waiting_reason"] != nil {
			data := map[string]interface{}{"state": job.State, "previous_state": previous}
			if job.WaitingReason != "" && job.State == agentcore.WorkJobStateWaiting {
				data["waiting_reason"] = job.WaitingReason
			}
			if job.BlockedRunID != "" {
				data["blocked_run_id"] = job.BlockedRunID
			}
			if job.Error != "" {
				data["error"] = job.Error
			}
			return appendWorkEvent(tx, job, "work_job.state_changed", data, now)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.workJobView(ctx, s.db.WithContext(ctx), job)
}

func (s *SQL) AddWorkItems(ctx context.Context, appID, jobID string, inputs []agentcore.WorkItemInput) (*agentcore.WorkJob, error) {
	now := time.Now().UTC()
	var job *workJobRecord
	err := s.workTx(ctx, func(tx *gorm.DB) error {
		var err error
		job, err = lockWorkJob(tx, appID, jobID, now)
		if err != nil {
			return err
		}
		if agentcore.IsTerminalWorkJobState(job.State) {
			return &agentcore.WorkRejectedError{Reason: "work job has ended"}
		}
		ids := make([]string, 0, len(inputs))
		for _, item := range inputs {
			ids = append(ids, item.ID)
		}
		var existing int64
		if err := tx.Model(&workItemRecord{}).Where("app_id = ? AND job_id = ? AND id IN ?", appID, jobID, ids).Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return &agentcore.WorkRejectedError{Reason: "item ids must be unique within the job"}
		}
		items, err := newWorkItemRecords(appID, jobID, job.TotalItems, inputs, now)
		if err != nil {
			return err
		}
		if err := tx.CreateInBatches(items, 200).Error; err != nil {
			return err
		}
		job.TotalItems += len(items)
		changes := map[string]interface{}{"total_items": job.TotalItems}
		// New items reopen a job that was verifying or waiting on coverage.
		if job.State == agentcore.WorkJobStateVerifying {
			job.State = agentcore.WorkJobStateRunning
			changes["state"] = job.State
		}
		if err := tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", appID, jobID).Updates(changes).Error; err != nil {
			return err
		}
		return appendWorkEvent(tx, job, "work_job.items_added", map[string]interface{}{"items": len(items)}, now)
	})
	if err != nil {
		return nil, err
	}
	return s.workJobView(ctx, s.db.WithContext(ctx), job)
}

func (s *SQL) ReviseWorkJob(ctx context.Context, appID, jobID string, expectedRevision int, spec agentcore.WorkJobSpec, now time.Time) (*agentcore.WorkJob, error) {
	now = nowOr(now)
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var job *workJobRecord
	err = s.workTx(ctx, func(tx *gorm.DB) error {
		job, err = lockWorkJob(tx, appID, jobID, now)
		if err != nil {
			return err
		}
		if agentcore.IsTerminalWorkJobState(job.State) {
			return &agentcore.WorkRejectedError{Reason: "work job has ended"}
		}
		if job.SpecRevision != expectedRevision {
			return agentcore.ErrWorkJobRevisionConflict
		}
		job.SpecRevision++
		job.Spec = raw
		if err := tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", appID, jobID).
			Updates(map[string]interface{}{"spec_revision": job.SpecRevision, "spec": jsonBytes(raw)}).Error; err != nil {
			return err
		}
		return appendWorkEvent(tx, job, "work_job.revised", map[string]interface{}{"spec_revision": job.SpecRevision}, now)
	})
	if err != nil {
		return nil, err
	}
	return s.workJobView(ctx, s.db.WithContext(ctx), job)
}

func (s *SQL) ListWorkItems(ctx context.Context, appID, jobID, state, after string, limit int) (*agentcore.WorkItemPage, error) {
	if !s.WorkJobsSupported() {
		return nil, agentcore.ErrWorkJobsUnsupported
	}
	if limit <= 0 {
		limit = defaultWorkItemPage
	}
	limit = min(limit, maxWorkItemPage)
	query := s.db.WithContext(ctx).Where("app_id = ? AND job_id = ?", appID, jobID)
	if state != "" {
		query = query.Where("state = ?", state)
	}
	if after != "" {
		var seq int
		if err := s.db.WithContext(ctx).Model(&workItemRecord{}).Select("seq").Where("app_id = ? AND job_id = ? AND id = ?", appID, jobID, after).Scan(&seq).Error; err != nil {
			return nil, err
		}
		query = query.Where("seq > ?", seq)
	}
	var records []workItemRecord
	if err := query.Order("seq").Limit(limit + 1).Find(&records).Error; err != nil {
		return nil, err
	}
	page := &agentcore.WorkItemPage{Items: make([]agentcore.WorkItem, 0, len(records))}
	if len(records) > limit {
		records = records[:limit]
		page.NextAfter = records[limit-1].ID
	}
	for _, record := range records {
		page.Items = append(page.Items, record.toCore())
	}
	return page, nil
}

func (s *SQL) RequeueWorkItems(ctx context.Context, appID, jobID string, itemIDs []string, reason string, now time.Time) error {
	if len(itemIDs) == 0 {
		return nil
	}
	now = nowOr(now)
	return s.workTx(ctx, func(tx *gorm.DB) error {
		job, err := lockWorkJob(tx, appID, jobID, now)
		if err != nil {
			return err
		}
		result := tx.Model(&workItemRecord{}).Where("app_id = ? AND job_id = ? AND id IN ? AND state = ?", appID, jobID, itemIDs, agentcore.WorkItemStateCompleted).
			Updates(map[string]interface{}{"state": agentcore.WorkItemStatePending, "outcome": "", "result": nil, "exception_kind": "", "exception_detail": "",
				"result_digest": "", "attempts": 0, "attempt_id": "", "run_id": "", "last_error": reason, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if job.State == agentcore.WorkJobStateVerifying {
			if err := tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", appID, jobID).Update("state", agentcore.WorkJobStateRunning).Error; err != nil {
				return err
			}
		}
		return appendWorkEvent(tx, job, "work_job.items_requeued", map[string]interface{}{"items": result.RowsAffected, "reason": reason}, now)
	})
}

func (s *SQL) ClaimWorkBatch(ctx context.Context, claim agentcore.WorkClaim) (*agentcore.WorkAttempt, []agentcore.WorkItem, error) {
	now := nowOr(claim.Now)
	if claim.BatchSize <= 0 {
		return nil, nil, fmt.Errorf("batch size must be positive")
	}
	var attempt *workAttemptRecord
	var claimed []workItemRecord
	err := s.workTx(ctx, func(tx *gorm.DB) error {
		attempt, claimed = nil, nil
		job, err := lockWorkJob(tx, claim.AppID, claim.JobID, now)
		if err != nil {
			return err
		}
		if job.State != agentcore.WorkJobStateQueued && job.State != agentcore.WorkJobStateRunning {
			return nil
		}
		var spec agentcore.WorkJobSpec
		if err := json.Unmarshal(job.Spec, &spec); err != nil {
			return err
		}
		if max := spec.Budget.MaxTokens; max > 0 && job.InputTokens+job.OutputTokens+job.ReservedTokens+claim.Reserve > max {
			return nil
		}
		items, err := eligibleWorkItems(tx, claim.AppID, claim.JobID, claim.BatchSize, now)
		if err != nil || len(items) == 0 {
			return err
		}
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		rawIDs, _ := json.Marshal(ids)
		attempt = &workAttemptRecord{AppID: claim.AppID, ID: claim.AttemptID, JobID: claim.JobID, State: agentcore.WorkAttemptStateOpen,
			ItemIDs: string(rawIDs), Reserved: claim.Reserve, UsesBrowser: claim.UsesBrowser, StartedAt: now}
		if err := tx.Create(attempt).Error; err != nil {
			return err
		}
		if err := tx.Model(&workItemRecord{}).Where("app_id = ? AND job_id = ? AND id IN ?", claim.AppID, claim.JobID, ids).
			// A new assignment starts without a result digest: an earlier
			// attempt's identical submission is not a replay of this one.
			Updates(map[string]interface{}{"state": agentcore.WorkItemStateRunning, "attempt_id": attempt.ID, "run_id": "", "result_digest": "",
				"attempts": gorm.Expr("attempts + 1"), "retry_at": nil, "updated_at": now}).Error; err != nil {
			return err
		}
		changes := map[string]interface{}{"reserved_tokens": job.ReservedTokens + claim.Reserve}
		if job.State == agentcore.WorkJobStateQueued {
			changes["state"] = agentcore.WorkJobStateRunning
		}
		if err := tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", claim.AppID, claim.JobID).Updates(changes).Error; err != nil {
			return err
		}
		if job.State == agentcore.WorkJobStateQueued {
			if err := appendWorkEvent(tx, job, "work_job.state_changed", map[string]interface{}{"state": agentcore.WorkJobStateRunning, "previous_state": job.State}, now); err != nil {
				return err
			}
		}
		if err := appendWorkEvent(tx, job, "work_job.attempt_started", map[string]interface{}{"attempt_id": attempt.ID, "items": ids}, now); err != nil {
			return err
		}
		return tx.Where("app_id = ? AND job_id = ? AND id IN ?", claim.AppID, claim.JobID, ids).Order("seq").Find(&claimed).Error
	})
	if err != nil || attempt == nil {
		return nil, nil, err
	}
	out := make([]agentcore.WorkItem, 0, len(claimed))
	for _, item := range claimed {
		out = append(out, item.toCore())
	}
	view := attempt.toCore()
	return &view, out, nil
}

// eligibleWorkItems selects pending items and items whose retry time has
// passed, in input order, skipping items whose dependencies have not all
// completed successfully.
func eligibleWorkItems(tx *gorm.DB, appID, jobID string, limit int, now time.Time) ([]workItemRecord, error) {
	var out []workItemRecord
	var lastSeq int
	for len(out) < limit {
		var candidates []workItemRecord
		if err := tx.Where("app_id = ? AND job_id = ? AND seq > ? AND (state = ? OR (state = ? AND (retry_at IS NULL OR retry_at <= ?)))",
			appID, jobID, lastSeq, agentcore.WorkItemStatePending, agentcore.WorkItemStateRetryWait, now).
			Order("seq").Limit(limit * 4).Find(&candidates).Error; err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			return out, nil
		}
		lastSeq = candidates[len(candidates)-1].Seq
		ready, err := dependencyReady(tx, appID, jobID, candidates)
		if err != nil {
			return nil, err
		}
		for _, item := range candidates {
			if ready[item.ID] && len(out) < limit {
				out = append(out, item)
			}
		}
	}
	return out, nil
}

func dependencyReady(tx *gorm.DB, appID, jobID string, items []workItemRecord) (map[string]bool, error) {
	ready := map[string]bool{}
	var deps []string
	for _, item := range items {
		for _, dep := range item.dependencies() {
			if !slices.Contains(deps, dep) {
				deps = append(deps, dep)
			}
		}
	}
	satisfied := map[string]bool{}
	if len(deps) > 0 {
		var rows []workItemRecord
		if err := tx.Select("id", "state", "outcome").Where("app_id = ? AND job_id = ? AND id IN ?", appID, jobID, deps).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			satisfied[row.ID] = row.State == agentcore.WorkItemStateCompleted && row.Outcome != agentcore.WorkItemOutcomeException
		}
	}
	for _, item := range items {
		ok := true
		for _, dep := range item.dependencies() {
			ok = ok && satisfied[dep]
		}
		ready[item.ID] = ok
	}
	return ready, nil
}

func (s *SQL) SetWorkAttemptRun(ctx context.Context, appID, attemptID, runID string, generation int64) error {
	return s.workTx(ctx, func(tx *gorm.DB) error {
		var attempt workAttemptRecord
		if err := tx.Where("app_id = ? AND id = ?", appID, attemptID).Take(&attempt).Error; err != nil {
			return err
		}
		if _, err := lockWorkJob(tx, appID, attempt.JobID, time.Now().UTC()); err != nil {
			return err
		}
		if attempt.RunID != "" && attempt.RunID != runID {
			return fmt.Errorf("attempt %s already belongs to run %s", attemptID, attempt.RunID)
		}
		if err := tx.Model(&workAttemptRecord{}).Where("app_id = ? AND id = ?", appID, attemptID).
			Updates(map[string]interface{}{"run_id": runID, "generation": generation}).Error; err != nil {
			return err
		}
		return tx.Model(&workItemRecord{}).Where("app_id = ? AND job_id = ? AND attempt_id = ? AND state = ?", appID, attempt.JobID, attemptID, agentcore.WorkItemStateRunning).
			Update("run_id", runID).Error
	})
}

func (s *SQL) GetWorkAttempt(ctx context.Context, appID, attemptID string) (*agentcore.WorkAttempt, error) {
	return s.takeWorkAttempt(ctx, "app_id = ? AND id = ?", appID, attemptID)
}

func (s *SQL) GetWorkAttemptByRun(ctx context.Context, appID, runID string) (*agentcore.WorkAttempt, error) {
	if runID == "" {
		return nil, nil
	}
	return s.takeWorkAttempt(ctx, "app_id = ? AND run_id = ?", appID, runID)
}

func (s *SQL) takeWorkAttempt(ctx context.Context, where string, args ...interface{}) (*agentcore.WorkAttempt, error) {
	if !s.WorkJobsSupported() {
		return nil, agentcore.ErrWorkJobsUnsupported
	}
	var record workAttemptRecord
	err := s.db.WithContext(ctx).Where(where, args...).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	view := record.toCore()
	return &view, nil
}

func (s *SQL) ListWorkAttempts(ctx context.Context, appID, jobID string, openOnly bool) ([]agentcore.WorkAttempt, error) {
	if !s.WorkJobsSupported() {
		return nil, agentcore.ErrWorkJobsUnsupported
	}
	query := s.db.WithContext(ctx).Where("app_id = ? AND job_id = ?", appID, jobID)
	if openOnly {
		query = query.Where("state = ?", agentcore.WorkAttemptStateOpen)
	}
	var records []workAttemptRecord
	if err := query.Order("started_at").Order("id").Find(&records).Error; err != nil {
		return nil, err
	}
	out := make([]agentcore.WorkAttempt, 0, len(records))
	for _, record := range records {
		out = append(out, record.toCore())
	}
	return out, nil
}

func (s *SQL) AcceptWorkResult(ctx context.Context, result agentcore.WorkResult) (*agentcore.WorkReceipt, error) {
	now := nowOr(result.Now)
	var receipt *agentcore.WorkReceipt
	err := s.workTx(ctx, func(tx *gorm.DB) error {
		receipt = nil
		var attempt workAttemptRecord
		err := tx.Where("app_id = ? AND id = ?", result.AppID, result.AttemptID).Take(&attempt).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &agentcore.WorkRejectedError{Reason: "this run has no work assignment"}
		}
		if err != nil {
			return err
		}
		job, err := lockWorkJob(tx, result.AppID, attempt.JobID, now)
		if err != nil {
			return err
		}
		if attempt.RunID != result.RunID {
			return &agentcore.WorkRejectedError{Reason: "this run does not own the assignment"}
		}
		var item workItemRecord
		err = tx.Where("app_id = ? AND job_id = ? AND id = ?", result.AppID, attempt.JobID, result.ItemID).Take(&item).Error
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && !attempt.assigned(result.ItemID)) {
			return &agentcore.WorkRejectedError{Reason: fmt.Sprintf("item %q is not assigned to this batch", result.ItemID)}
		}
		if err != nil {
			return err
		}
		// An identical replay from the same attempt returns its receipt even
		// after the attempt closed; nothing is written again.
		if item.AttemptID == attempt.ID && item.ResultDigest == result.Digest && item.ResultDigest != "" {
			status := agentcore.WorkReceiptAlreadyAccepted
			if item.State == agentcore.WorkItemStateRetryWait {
				status = agentcore.WorkReceiptRetryScheduled
			}
			receipt = &agentcore.WorkReceipt{ItemID: item.ID, Status: status, Outcome: item.Outcome, SpecRevision: item.SpecRevision}
			return nil
		}
		if attempt.State != agentcore.WorkAttemptStateOpen {
			return &agentcore.WorkRejectedError{Reason: "this batch attempt has ended"}
		}
		var generation int64
		if err := tx.Model(&runGroupMemberRecord{}).Select("execution_generation").Where("app_id = ? AND run_id = ?", result.AppID, result.RunID).Scan(&generation).Error; err != nil {
			return err
		}
		if generation != attempt.Generation {
			return &agentcore.WorkRejectedError{Reason: "this run's execution authority was replaced; stop and finish the turn"}
		}
		if item.State == agentcore.WorkItemStateCompleted {
			return &agentcore.WorkRejectedError{Reason: fmt.Sprintf("item %q already has an accepted result; accepted results cannot be replaced", item.ID)}
		}
		if item.AttemptID != attempt.ID || item.State != agentcore.WorkItemStateRunning {
			return &agentcore.WorkRejectedError{Reason: fmt.Sprintf("item %q is not assigned to this batch", result.ItemID)}
		}
		var spec agentcore.WorkJobSpec
		if err := json.Unmarshal(job.Spec, &spec); err != nil {
			return err
		}
		changes := map[string]interface{}{"outcome": result.Outcome, "result": nullableJSON(result.Result), "result_digest": result.Digest,
			"spec_revision": job.SpecRevision, "exception_kind": "", "exception_detail": "", "last_error": "", "updated_at": now}
		if result.Exception != nil {
			changes["exception_kind"], changes["exception_detail"] = result.Exception.Kind, result.Exception.Detail
		}
		status := agentcore.WorkReceiptAccepted
		if result.Retryable && item.Attempts < maxAttemptsPerItem(spec) {
			status = agentcore.WorkReceiptRetryScheduled
			retryAt := now.Add(result.RetryAfter)
			changes["state"], changes["retry_at"] = agentcore.WorkItemStateRetryWait, retryAt
			changes["last_error"] = result.Exception.Kind + ": " + result.Exception.Detail
		} else {
			changes["state"] = agentcore.WorkItemStateCompleted
		}
		if err := tx.Model(&workItemRecord{}).Where("app_id = ? AND job_id = ? AND id = ?", result.AppID, attempt.JobID, item.ID).Updates(changes).Error; err != nil {
			return err
		}
		if err := tx.Model(&workAttemptRecord{}).Where("app_id = ? AND id = ?", result.AppID, attempt.ID).UpdateColumn("accepted", gorm.Expr("accepted + 1")).Error; err != nil {
			return err
		}
		var remaining int64
		if err := tx.Model(&workItemRecord{}).Where("app_id = ? AND job_id = ? AND attempt_id = ? AND state = ?", result.AppID, attempt.JobID, attempt.ID, agentcore.WorkItemStateRunning).
			Count(&remaining).Error; err != nil {
			return err
		}
		receipt = &agentcore.WorkReceipt{ItemID: item.ID, Status: status, Outcome: result.Outcome, SpecRevision: job.SpecRevision, Remaining: int(remaining)}
		eventType := "work_item.accepted"
		if status == agentcore.WorkReceiptRetryScheduled {
			eventType = "work_item.retry_scheduled"
		}
		return appendWorkEvent(tx, job, eventType, map[string]interface{}{"item_id": item.ID, "outcome": result.Outcome, "attempt_id": attempt.ID, "run_id": result.RunID}, now)
	})
	return receipt, err
}

func maxAttemptsPerItem(spec agentcore.WorkJobSpec) int {
	if spec.MaxAttemptsPerItem > 0 {
		return spec.MaxAttemptsPerItem
	}
	return 3
}

func nullableJSON(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

func (s *SQL) ApplyWorkAttemptUsage(ctx context.Context, close agentcore.WorkAttemptClose) error {
	now := nowOr(close.Now)
	return s.workTx(ctx, func(tx *gorm.DB) error {
		attempt, job, err := lockAttempt(tx, close.AppID, close.AttemptID, now)
		if err != nil {
			return err
		}
		if attempt.State != agentcore.WorkAttemptStateOpen {
			return nil
		}
		return applyAttemptUsage(tx, job, attempt, close, false)
	})
}

func lockAttempt(tx *gorm.DB, appID, attemptID string, now time.Time) (*workAttemptRecord, *workJobRecord, error) {
	var attempt workAttemptRecord
	if err := tx.Where("app_id = ? AND id = ?", appID, attemptID).Take(&attempt).Error; err != nil {
		return nil, nil, err
	}
	job, err := lockWorkJob(tx, appID, attempt.JobID, now)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Where("app_id = ? AND id = ?", appID, attemptID).Take(&attempt).Error; err != nil {
		return nil, nil, err
	}
	return &attempt, job, nil
}

// applyAttemptUsage moves the attempt's watermark to the run's cumulative
// counters and adds only the difference to the job. Counters never move
// backwards, so replays and duplicate notifications add nothing. A final
// application releases the attempt's reservation.
func applyAttemptUsage(tx *gorm.DB, job *workJobRecord, attempt *workAttemptRecord, close agentcore.WorkAttemptClose, final bool) error {
	input := max(close.InputTokens, attempt.UsageInput)
	output := max(close.OutputTokens, attempt.UsageOutput)
	cached := max(close.CachedTokens, attempt.UsageCached)
	job.InputTokens += input - attempt.UsageInput
	job.OutputTokens += output - attempt.UsageOutput
	job.CachedTokens += cached - attempt.UsageCached
	attempt.UsageInput, attempt.UsageOutput, attempt.UsageCached = input, output, cached
	changes := map[string]interface{}{"input_tokens": job.InputTokens, "output_tokens": job.OutputTokens, "cached_tokens": job.CachedTokens}
	if final {
		job.ReservedTokens = max(0, job.ReservedTokens-attempt.Reserved)
		changes["reserved_tokens"] = job.ReservedTokens
	}
	if err := tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", job.AppID, job.ID).Updates(changes).Error; err != nil {
		return err
	}
	return tx.Model(&workAttemptRecord{}).Where("app_id = ? AND id = ?", attempt.AppID, attempt.ID).
		Updates(map[string]interface{}{"usage_input": input, "usage_output": output, "usage_cached": cached}).Error
}

func (s *SQL) CloseWorkAttempt(ctx context.Context, close agentcore.WorkAttemptClose) (*agentcore.WorkAttempt, error) {
	now := nowOr(close.Now)
	var out *workAttemptRecord
	err := s.workTx(ctx, func(tx *gorm.DB) error {
		attempt, job, err := lockAttempt(tx, close.AppID, close.AttemptID, now)
		if err != nil {
			return err
		}
		out = attempt
		if attempt.State != agentcore.WorkAttemptStateOpen {
			return nil
		}
		if err := applyAttemptUsage(tx, job, attempt, close, true); err != nil {
			return err
		}
		var spec agentcore.WorkJobSpec
		if err := json.Unmarshal(job.Spec, &spec); err != nil {
			return err
		}
		var unresolved []workItemRecord
		if err := tx.Where("app_id = ? AND job_id = ? AND attempt_id = ? AND state = ?", close.AppID, attempt.JobID, attempt.ID, agentcore.WorkItemStateRunning).
			Find(&unresolved).Error; err != nil {
			return err
		}
		state := close.State
		if state == agentcore.WorkAttemptStateCompleted && len(unresolved) > 0 {
			// A worker that finished with unresolved items did not complete
			// its batch, whatever it claimed.
			state = agentcore.WorkAttemptStatePartial
		}
		reason := close.Outcome
		if reason == "" {
			reason = "the worker ended without a result for this item"
		}
		for _, item := range unresolved {
			next := agentcore.WorkItemStateRetryWait
			if close.State == agentcore.WorkAttemptStateCancelled {
				next = agentcore.WorkItemStatePending
			} else if item.Attempts >= maxAttemptsPerItem(spec) {
				next = agentcore.WorkItemStateFailed
			}
			if err := tx.Model(&workItemRecord{}).Where("app_id = ? AND job_id = ? AND id = ?", close.AppID, attempt.JobID, item.ID).
				Updates(map[string]interface{}{"state": next, "last_error": reason, "retry_at": now, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		var acceptedNow int64
		if err := tx.Model(&workAttemptRecord{}).Select("accepted").Where("app_id = ? AND id = ?", close.AppID, attempt.ID).Scan(&acceptedNow).Error; err != nil {
			return err
		}
		attempt.Accepted = int(acceptedNow)
		if close.State != agentcore.WorkAttemptStateCancelled {
			if attempt.Accepted == 0 {
				job.NoProgress++
			} else {
				job.NoProgress = 0
			}
			if err := tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", job.AppID, job.ID).UpdateColumn("no_progress", job.NoProgress).Error; err != nil {
				return err
			}
		}
		attempt.State, attempt.Outcome, attempt.EndedAt = state, close.Outcome, &now
		if err := tx.Model(&workAttemptRecord{}).Where("app_id = ? AND id = ?", close.AppID, attempt.ID).
			Updates(map[string]interface{}{"state": state, "outcome": close.Outcome, "ended_at": now}).Error; err != nil {
			return err
		}
		return appendWorkEvent(tx, job, "work_job.attempt_ended", map[string]interface{}{
			"attempt_id": attempt.ID, "run_id": attempt.RunID, "state": state, "accepted": attempt.Accepted,
			"unresolved": len(unresolved), "tokens": attempt.UsageInput + attempt.UsageOutput,
		}, now)
	})
	if err != nil {
		return nil, err
	}
	view := out.toCore()
	return &view, nil
}

func (s *SQL) WorkJobProgress(ctx context.Context, appID, jobID string) (int, int, error) {
	var record workJobRecord
	err := s.db.WithContext(ctx).Select("no_progress", "accepted_exceptions").Where("app_id = ? AND id = ?", appID, jobID).Take(&record).Error
	return record.NoProgress, record.AcceptedExceptions, err
}

func (s *SQL) AppendWorkJobEvent(ctx context.Context, appID, jobID, eventType string, data map[string]interface{}) error {
	now := time.Now().UTC()
	return s.workTx(ctx, func(tx *gorm.DB) error {
		job, err := lockWorkJob(tx, appID, jobID, now)
		if err != nil {
			return err
		}
		return appendWorkEvent(tx, job, eventType, data, now)
	})
}

func (s *SQL) ListWorkJobEvents(ctx context.Context, appID, jobID string, after int64, limit int) (*agentcore.WorkJobEventPage, error) {
	if !s.WorkJobsSupported() {
		return nil, agentcore.ErrWorkJobsUnsupported
	}
	if limit <= 0 || limit > maxWorkJobEvents {
		limit = maxWorkJobEvents
	}
	var records []workJobEventRecord
	if err := s.db.WithContext(ctx).Where("app_id = ? AND job_id = ? AND seq > ?", appID, jobID, after).Order("seq").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	page := &agentcore.WorkJobEventPage{Items: make([]agentcore.WorkJobEvent, 0, len(records)), NextAfter: after}
	for _, record := range records {
		event := agentcore.WorkJobEvent{Seq: record.Seq, JobID: record.JobID, Type: record.Type, CreatedAt: record.CreatedAt}
		_ = json.Unmarshal(record.Data, &event.Data)
		page.Items = append(page.Items, event)
		page.NextAfter = record.Seq
	}
	return page, nil
}

func (s *SQL) PublishWorkJobDataset(ctx context.Context, appID, jobID string, now time.Time) (*agentcore.WorkJobDataset, error) {
	now = nowOr(now)
	var dataset *agentcore.WorkJobDataset
	err := s.workTx(ctx, func(tx *gorm.DB) error {
		job, err := lockWorkJob(tx, appID, jobID, now)
		if err != nil {
			return err
		}
		var records []workItemRecord
		if err := tx.Where("app_id = ? AND job_id = ? AND state = ?", appID, jobID, agentcore.WorkItemStateCompleted).Order("seq").Find(&records).Error; err != nil {
			return err
		}
		items := make([]agentcore.WorkDatasetItem, 0, len(records))
		for _, record := range records {
			item := agentcore.WorkDatasetItem{ID: record.ID, Outcome: record.Outcome, SpecRevision: record.SpecRevision}
			if len(record.Result) > 0 {
				item.Result = json.RawMessage(record.Result)
			}
			if record.ExceptionKind != "" {
				item.Exception = &agentcore.WorkItemException{Kind: record.ExceptionKind, Detail: record.ExceptionDetail}
			}
			items = append(items, item)
		}
		raw, err := json.Marshal(items)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		var latest workDatasetRecord
		err = tx.Where("app_id = ? AND job_id = ?", appID, jobID).Order("revision DESC").Take(&latest).Error
		if err == nil && latest.Digest == digest {
			dataset = latest.toCore()
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		record := workDatasetRecord{AppID: appID, JobID: jobID, Revision: job.DatasetRevision + 1, Digest: digest, Items: raw, CreatedAt: now}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		job.DatasetRevision = record.Revision
		if err := tx.Model(&workJobRecord{}).Where("app_id = ? AND id = ?", appID, jobID).UpdateColumn("dataset_revision", record.Revision).Error; err != nil {
			return err
		}
		dataset = record.toCore()
		return appendWorkEvent(tx, job, "work_job.dataset_published", map[string]interface{}{"revision": record.Revision, "items": len(items), "digest": digest}, now)
	})
	return dataset, err
}

func (s *SQL) GetWorkJobDataset(ctx context.Context, appID, jobID string) (*agentcore.WorkJobDataset, error) {
	if !s.WorkJobsSupported() {
		return nil, agentcore.ErrWorkJobsUnsupported
	}
	var record workDatasetRecord
	err := s.db.WithContext(ctx).Where("app_id = ? AND job_id = ?", appID, jobID).Order("revision DESC").Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return record.toCore(), nil
}

func (r workDatasetRecord) toCore() *agentcore.WorkJobDataset {
	dataset := &agentcore.WorkJobDataset{JobID: r.JobID, Revision: r.Revision, Digest: r.Digest, Created: r.CreatedAt, Items: []agentcore.WorkDatasetItem{}}
	_ = json.Unmarshal(r.Items, &dataset.Items)
	return dataset
}

func (r workItemRecord) dependencies() []string {
	var deps []string
	_ = json.Unmarshal([]byte(r.DependsOn), &deps)
	return deps
}

func (r workItemRecord) toCore() agentcore.WorkItem {
	item := agentcore.WorkItem{
		ID: r.ID, JobID: r.JobID, State: r.State, Input: json.RawMessage(r.Input), DependsOn: r.dependencies(), Attempts: r.Attempts,
		Outcome: r.Outcome, SpecRevision: r.SpecRevision, AttemptID: r.AttemptID, RunID: r.RunID, LastError: r.LastError, UpdatedAt: r.UpdatedAt,
	}
	if len(r.Result) > 0 {
		item.Result = json.RawMessage(r.Result)
	}
	if r.ExceptionKind != "" {
		item.Exception = &agentcore.WorkItemException{Kind: r.ExceptionKind, Detail: r.ExceptionDetail}
	}
	return item
}

func (r workAttemptRecord) itemIDs() []string {
	var ids []string
	_ = json.Unmarshal([]byte(r.ItemIDs), &ids)
	return ids
}

func (r workAttemptRecord) assigned(itemID string) bool { return slices.Contains(r.itemIDs(), itemID) }

func (r workAttemptRecord) toCore() agentcore.WorkAttempt {
	return agentcore.WorkAttempt{
		ID: r.ID, JobID: r.JobID, RunID: r.RunID, State: r.State, ItemIDs: r.itemIDs(), Accepted: r.Accepted,
		Generation: r.Generation, Outcome: r.Outcome, Tokens: r.UsageInput + r.UsageOutput, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
	}
}

func nowOr(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func (s *SQL) GetWorkItems(ctx context.Context, appID, jobID string, itemIDs []string) ([]agentcore.WorkItem, error) {
	if !s.WorkJobsSupported() {
		return nil, agentcore.ErrWorkJobsUnsupported
	}
	var records []workItemRecord
	if err := s.db.WithContext(ctx).Where("app_id = ? AND job_id = ? AND id IN ?", appID, jobID, itemIDs).Order("seq").Find(&records).Error; err != nil {
		return nil, err
	}
	out := make([]agentcore.WorkItem, 0, len(records))
	for _, record := range records {
		out = append(out, record.toCore())
	}
	return out, nil
}

func (s *SQL) CancelWorkItems(ctx context.Context, appID, jobID string, now time.Time) error {
	now = nowOr(now)
	return s.workTx(ctx, func(tx *gorm.DB) error {
		if _, err := lockWorkJob(tx, appID, jobID, now); err != nil {
			return err
		}
		return tx.Model(&workItemRecord{}).Where("app_id = ? AND job_id = ? AND state IN ?", appID, jobID,
			[]string{agentcore.WorkItemStatePending, agentcore.WorkItemStateRetryWait, agentcore.WorkItemStateRunning, agentcore.WorkItemStateBlocked}).
			Updates(map[string]interface{}{"state": agentcore.WorkItemStateCancelled, "updated_at": now}).Error
	})
}

// FailBlockedWorkItems fails pending items with a prerequisite that ended
// failed, cancelled or as an exception; repeated until no more items fail,
// so failure propagates along dependency chains.
func (s *SQL) FailBlockedWorkItems(ctx context.Context, appID, jobID string, now time.Time) (int, error) {
	now = nowOr(now)
	failed := 0
	err := s.workTx(ctx, func(tx *gorm.DB) error {
		failed = 0
		job, err := lockWorkJob(tx, appID, jobID, now)
		if err != nil {
			return err
		}
		for {
			var pending []workItemRecord
			if err := tx.Where("app_id = ? AND job_id = ? AND state IN ? AND depends_on <> '[]'", appID, jobID,
				[]string{agentcore.WorkItemStatePending, agentcore.WorkItemStateRetryWait}).Find(&pending).Error; err != nil {
				return err
			}
			if len(pending) == 0 {
				return nil
			}
			var deps []string
			for _, item := range pending {
				deps = append(deps, item.dependencies()...)
			}
			var rows []workItemRecord
			if err := tx.Select("id", "state", "outcome").Where("app_id = ? AND job_id = ? AND id IN ?", appID, jobID, deps).Find(&rows).Error; err != nil {
				return err
			}
			dead := map[string]bool{}
			for _, row := range rows {
				dead[row.ID] = row.State == agentcore.WorkItemStateFailed || row.State == agentcore.WorkItemStateCancelled ||
					(row.State == agentcore.WorkItemStateCompleted && row.Outcome == agentcore.WorkItemOutcomeException)
			}
			round := 0
			for _, item := range pending {
				for _, dep := range item.dependencies() {
					if dead[dep] {
						if err := tx.Model(&workItemRecord{}).Where("app_id = ? AND job_id = ? AND id = ?", appID, jobID, item.ID).
							Updates(map[string]interface{}{"state": agentcore.WorkItemStateFailed, "last_error": "prerequisite " + dep + " did not succeed", "updated_at": now}).Error; err != nil {
							return err
						}
						round++
						break
					}
				}
			}
			if round == 0 {
				break
			}
			failed += round
		}
		if failed > 0 {
			return appendWorkEvent(tx, job, "work_job.items_failed", map[string]interface{}{"items": failed, "reason": "prerequisite did not succeed"}, now)
		}
		return nil
	})
	return failed, err
}

var _ agentcore.WorkJobStore = (*SQL)(nil)
