package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/id"
)

// Run group tables are additive. agent_runs is never altered: SQL.UpdateRun
// saves the full run record, so new run columns would break stores whose
// schema has not been migrated. Stores without these tables never query them.

type runGroupRecord struct {
	AppID             string    `gorm:"primaryKey;uniqueIndex:idx_run_groups_creation,priority:1"`
	ID                string    `gorm:"primaryKey"`
	CreationKey       *string   `gorm:"uniqueIndex:idx_run_groups_creation,priority:2"`
	OwnerKind         string    `gorm:"not null"`
	ParentRunID       string    `gorm:"not null;default:''"`
	State             string    `gorm:"not null"`
	Policy            jsonBytes `gorm:"type:json"`
	LastCompletionSeq int64     `gorm:"not null;default:0"`
	CreatedAt         time.Time `gorm:"not null"`
	UpdatedAt         time.Time `gorm:"not null"`
}

func (runGroupRecord) TableName() string { return "agent_run_groups" }

type runGroupMemberRecord struct {
	AppID               string `gorm:"primaryKey;index:idx_run_group_members_group,priority:1;index:idx_run_group_members_completion,priority:1"`
	RunID               string `gorm:"primaryKey"`
	GroupID             string `gorm:"not null;index:idx_run_group_members_group,priority:2;index:idx_run_group_members_completion,priority:2"`
	ParentRunID         string `gorm:"not null;default:''"`
	ExecutionGeneration int64  `gorm:"not null;default:0"`
	// CompletionSeq is zero until the member's first terminal status.
	CompletionSeq  int64  `gorm:"not null;default:0;index:idx_run_group_members_completion,priority:3"`
	TerminalStatus string `gorm:"not null;default:''"`
	CompletedAt    *time.Time
	Hidden         bool      `gorm:"not null;default:false"`
	CreatedAt      time.Time `gorm:"not null"`
}

func (runGroupMemberRecord) TableName() string { return "agent_run_group_members" }

var activeRunStatuses = []string{agentcore.RunStatusQueued, agentcore.RunStatusRunning, agentcore.RunStatusPaused}

func (s *SQL) RunGroupsSupported() bool { return s != nil && s.runGroups }

func (s *SQL) migrateRunGroups() error {
	if err := s.db.AutoMigrate(&runGroupRecord{}, &runGroupMemberRecord{}); err != nil {
		return fmt.Errorf("migrate run groups: %w", err)
	}
	s.runGroups = true
	return nil
}

func (s *SQL) CreateRunGroup(ctx context.Context, group *agentcore.RunGroup) (*agentcore.RunGroup, error) {
	if !s.RunGroupsSupported() {
		return nil, agentcore.ErrRunGroupsUnsupported
	}
	if group == nil || strings.TrimSpace(group.AppID) == "" {
		return nil, fmt.Errorf("run group app_id is required")
	}
	record := runGroupRecord{
		AppID:       strings.TrimSpace(group.AppID),
		ID:          strings.TrimSpace(group.ID),
		OwnerKind:   strings.TrimSpace(group.OwnerKind),
		ParentRunID: strings.TrimSpace(group.ParentRunID),
		State:       agentcore.RunGroupStateActive,
	}
	if record.ID == "" {
		record.ID = id.New("rungroup")
	}
	if record.OwnerKind == "" {
		record.OwnerKind = agentcore.RunGroupOwnerHost
	}
	if record.OwnerKind != agentcore.RunGroupOwnerHost && record.OwnerKind != agentcore.RunGroupOwnerWorkJob {
		return nil, fmt.Errorf("unsupported run group owner %q", record.OwnerKind)
	}
	if key := strings.TrimSpace(group.CreationKey); key != "" {
		record.CreationKey = &key
	}
	policy, err := json.Marshal(group.Policy)
	if err != nil {
		return nil, err
	}
	record.Policy = policy
	now := time.Now().UTC()
	record.CreatedAt, record.UpdatedAt = now, now
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if record.CreationKey != nil {
			var existing runGroupRecord
			err := tx.Where("app_id = ? AND creation_key = ?", record.AppID, *record.CreationKey).Take(&existing).Error
			if err == nil {
				record = existing
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		return tx.Create(&record).Error
	})
	if err != nil {
		return nil, err
	}
	return record.toCore(), nil
}

func (s *SQL) GetRunGroup(ctx context.Context, appID, groupID string) (*agentcore.RunGroup, error) {
	if !s.RunGroupsSupported() {
		return nil, agentcore.ErrRunGroupsUnsupported
	}
	record, err := takeRunGroup(s.db.WithContext(ctx), appID, groupID)
	if err != nil {
		return nil, err
	}
	return record.toCore(), nil
}

// SetRunGroupState moves active<->paused and any state to cancelled.
// Cancelled is final.
func (s *SQL) SetRunGroupState(ctx context.Context, appID, groupID, state string) (*agentcore.RunGroup, error) {
	if !s.RunGroupsSupported() {
		return nil, agentcore.ErrRunGroupsUnsupported
	}
	if err := validRunGroupState(state); err != nil {
		return nil, err
	}
	var out *agentcore.RunGroup
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record, err := lockRunGroup(tx, appID, groupID)
		if err != nil {
			return err
		}
		if record.State == agentcore.RunGroupStateCancelled && state != agentcore.RunGroupStateCancelled {
			return agentcore.ErrRunGroupClosed
		}
		if record.State != state {
			record.State = state
			record.UpdatedAt = time.Now().UTC()
			if err := tx.Model(&runGroupRecord{}).Where("app_id = ? AND id = ?", record.AppID, record.ID).
				Updates(map[string]interface{}{"state": record.State, "updated_at": record.UpdatedAt}).Error; err != nil {
				return err
			}
		}
		out = record.toCore()
		return nil
	})
	return out, err
}

func (s *SQL) GetRunGroupMember(ctx context.Context, appID, runID string) (*agentcore.RunGroupMember, error) {
	if !s.RunGroupsSupported() {
		return nil, nil
	}
	var record runGroupMemberRecord
	err := s.db.WithContext(ctx).Where("app_id = ? AND run_id = ?", strings.TrimSpace(appID), strings.TrimSpace(runID)).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	statuses, err := s.runStatuses(ctx, record.AppID, []string{record.RunID})
	if err != nil {
		return nil, err
	}
	member := record.toCore(statuses[record.RunID])
	return &member, nil
}

func (s *SQL) ListRunGroupMembers(ctx context.Context, appID, groupID string) ([]agentcore.RunGroupMember, error) {
	if !s.RunGroupsSupported() {
		return nil, agentcore.ErrRunGroupsUnsupported
	}
	if _, err := takeRunGroup(s.db.WithContext(ctx), appID, groupID); err != nil {
		return nil, err
	}
	var records []runGroupMemberRecord
	if err := s.db.WithContext(ctx).Where("app_id = ? AND group_id = ?", strings.TrimSpace(appID), strings.TrimSpace(groupID)).
		Order("created_at ASC").Order("run_id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	runIDs := make([]string, 0, len(records))
	for _, record := range records {
		runIDs = append(runIDs, record.RunID)
	}
	statuses, err := s.runStatuses(ctx, strings.TrimSpace(appID), runIDs)
	if err != nil {
		return nil, err
	}
	out := make([]agentcore.RunGroupMember, 0, len(records))
	for _, record := range records {
		out = append(out, record.toCore(statuses[record.RunID]))
	}
	return out, nil
}

func (s *SQL) ListRunGroupCompletions(ctx context.Context, appID, groupID string, after int64, limit int) (*agentcore.RunGroupCompletionPage, error) {
	if !s.RunGroupsSupported() {
		return nil, agentcore.ErrRunGroupsUnsupported
	}
	if _, err := takeRunGroup(s.db.WithContext(ctx), appID, groupID); err != nil {
		return nil, err
	}
	if after < 0 {
		after = 0
	}
	var records []runGroupMemberRecord
	if err := s.db.WithContext(ctx).
		Where("app_id = ? AND group_id = ? AND completion_seq > ?", strings.TrimSpace(appID), strings.TrimSpace(groupID), after).
		Order("completion_seq ASC").Limit(agentcore.NormalizeRunGroupCompletionLimit(limit)).Find(&records).Error; err != nil {
		return nil, err
	}
	page := &agentcore.RunGroupCompletionPage{Items: make([]agentcore.RunGroupCompletion, 0, len(records)), NextAfter: after}
	for _, record := range records {
		page.Items = append(page.Items, record.toCompletion())
		page.NextAfter = record.CompletionSeq
	}
	return page, nil
}

func (s *SQL) BumpRunExecutionGeneration(ctx context.Context, appID, runID string) (int64, error) {
	if !s.RunGroupsSupported() {
		return 0, agentcore.ErrRunGroupsUnsupported
	}
	var generation int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&runGroupMemberRecord{}).Where("app_id = ? AND run_id = ?", strings.TrimSpace(appID), strings.TrimSpace(runID)).
			UpdateColumn("execution_generation", gorm.Expr("execution_generation + 1"))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("run is not a run group member")
		}
		return tx.Model(&runGroupMemberRecord{}).Select("execution_generation").
			Where("app_id = ? AND run_id = ?", strings.TrimSpace(appID), strings.TrimSpace(runID)).Scan(&generation).Error
	})
	return generation, err
}

// insertRunGroupMember runs inside the run-creation transaction, after the
// run row is written (which holds SQLite's write lock), so the admission
// checks and the membership row commit or roll back together with the run.
func (s *SQL) insertRunGroupMember(ctx context.Context, tx *gorm.DB, run *agentcore.AgentRun, now time.Time) error {
	membership, ok := agentcore.RunGroupMembershipFromContext(ctx)
	if !ok {
		return nil
	}
	if !s.RunGroupsSupported() {
		return agentcore.ErrRunGroupsUnsupported
	}
	group, err := lockRunGroup(tx, run.AppID, membership.GroupID)
	if err != nil {
		return err
	}
	if group.State != agentcore.RunGroupStateActive {
		return agentcore.ErrRunGroupClosed
	}
	var policy agentcore.RunGroupPolicy
	if len(group.Policy) > 0 {
		if err := json.Unmarshal(group.Policy, &policy); err != nil {
			return fmt.Errorf("decode run group policy: %w", err)
		}
	}
	if policy.MaxActiveRuns > 0 {
		var active int64
		if err := tx.Model(&runGroupMemberRecord{}).
			Joins("JOIN agent_runs ON agent_runs.app_id = agent_run_group_members.app_id AND agent_runs.id = agent_run_group_members.run_id").
			Where("agent_run_group_members.app_id = ? AND agent_run_group_members.group_id = ? AND agent_runs.status IN ?", group.AppID, group.ID, activeRunStatuses).
			Count(&active).Error; err != nil {
			return err
		}
		if active >= int64(policy.MaxActiveRuns) {
			return agentcore.ErrRunGroupFull
		}
	}
	return tx.Create(&runGroupMemberRecord{
		AppID:       run.AppID,
		RunID:       run.ID,
		GroupID:     group.ID,
		ParentRunID: membership.ParentRunID,
		Hidden:      group.OwnerKind != agentcore.RunGroupOwnerHost,
		CreatedAt:   now,
	}).Error
}

// assignRunGroupCompletion runs inside UpdateRun's transaction after the run
// row is saved. The first terminal save of a member increments its group's
// counter and records the member's sequence; later terminal saves are no-ops.
// Assigning under the group row (not a database sequence) keeps sequences
// commit-ordered, so a cursor reader never skips a member that commits later.
func assignRunGroupCompletion(tx *gorm.DB, run *agentcore.AgentRun) error {
	var member runGroupMemberRecord
	err := tx.Where("app_id = ? AND run_id = ?", run.AppID, run.ID).Take(&member).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if member.CompletionSeq > 0 {
		return nil
	}
	if err := tx.Model(&runGroupRecord{}).Where("app_id = ? AND id = ?", member.AppID, member.GroupID).
		UpdateColumn("last_completion_seq", gorm.Expr("last_completion_seq + 1")).Error; err != nil {
		return err
	}
	var seq int64
	if err := tx.Model(&runGroupRecord{}).Select("last_completion_seq").
		Where("app_id = ? AND id = ?", member.AppID, member.GroupID).Scan(&seq).Error; err != nil {
		return err
	}
	completedAt := time.Now().UTC()
	if run.CompletedAt != nil {
		completedAt = run.CompletedAt.UTC()
	}
	return tx.Model(&runGroupMemberRecord{}).Where("app_id = ? AND run_id = ? AND completion_seq = 0", member.AppID, member.RunID).
		Updates(map[string]interface{}{"completion_seq": seq, "terminal_status": run.Status, "completed_at": completedAt}).Error
}

// hiddenRunGroupMemberFilter keeps runtime-created runs out of public listings.
const hiddenRunGroupMemberFilter = "NOT EXISTS (SELECT 1 FROM agent_run_group_members m WHERE m.app_id = agent_runs.app_id AND m.run_id = agent_runs.id AND m.hidden = ?)"

func (s *SQL) runStatuses(ctx context.Context, appID string, runIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(runIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		ID     string
		Status string
	}
	if err := s.db.WithContext(ctx).Model(&runRecord{}).Select("id", "status").Where("app_id = ? AND id IN ?", appID, runIDs).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row.Status
	}
	return out, nil
}

// lockRunGroup reads a group holding its row until the transaction ends, so
// admission and state changes see and change the group one at a time.
// SQLite transactions already hold the database's write lock.
func lockRunGroup(tx *gorm.DB, appID, groupID string) (*runGroupRecord, error) {
	if tx.Dialector.Name() == "postgres" {
		tx = tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return takeRunGroup(tx, appID, groupID)
}

func takeRunGroup(db *gorm.DB, appID, groupID string) (*runGroupRecord, error) {
	var record runGroupRecord
	err := db.Where("app_id = ? AND id = ?", strings.TrimSpace(appID), strings.TrimSpace(groupID)).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, agentcore.ErrRunGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func validRunGroupState(state string) error {
	switch state {
	case agentcore.RunGroupStateActive, agentcore.RunGroupStatePaused, agentcore.RunGroupStateCancelled:
		return nil
	default:
		return fmt.Errorf("unsupported run group state %q", state)
	}
}

func (r runGroupRecord) toCore() *agentcore.RunGroup {
	group := &agentcore.RunGroup{
		ID:                r.ID,
		AppID:             r.AppID,
		OwnerKind:         r.OwnerKind,
		ParentRunID:       r.ParentRunID,
		State:             r.State,
		LastCompletionSeq: r.LastCompletionSeq,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
	if r.CreationKey != nil {
		group.CreationKey = *r.CreationKey
	}
	if len(r.Policy) > 0 {
		_ = json.Unmarshal(r.Policy, &group.Policy)
	}
	return group
}

func (r runGroupMemberRecord) toCore(status string) agentcore.RunGroupMember {
	return agentcore.RunGroupMember{
		RunID:               r.RunID,
		GroupID:             r.GroupID,
		ParentRunID:         r.ParentRunID,
		Status:              status,
		ExecutionGeneration: r.ExecutionGeneration,
		CompletionSeq:       r.CompletionSeq,
		Hidden:              r.Hidden,
	}
}

func (r runGroupMemberRecord) toCompletion() agentcore.RunGroupCompletion {
	completion := agentcore.RunGroupCompletion{Seq: r.CompletionSeq, RunID: r.RunID, Status: r.TerminalStatus}
	if r.CompletedAt != nil {
		completion.CompletedAt = *r.CompletedAt
	}
	return completion
}
