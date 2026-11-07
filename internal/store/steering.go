package store

import (
	"context"
	"encoding/json"
	"errors"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

type runSteeringRecord struct {
	AppID     string    `gorm:"primaryKey"`
	RunID     string    `gorm:"primaryKey"`
	CommandID string    `gorm:"primaryKey"`
	Revision  int64     `gorm:"not null"`
	Request   jsonBytes `gorm:"type:json;not null"`
	State     string    `gorm:"not null"`
	CreatedAt time.Time
	AppliedAt *time.Time
}

func (runSteeringRecord) TableName() string { return "agent_run_steering" }
func (r runSteeringRecord) command() (sdk.RunSteeringCommand, error) {
	out := sdk.RunSteeringCommand{RunID: r.RunID, Revision: r.Revision, State: r.State, CreatedAt: r.CreatedAt, AppliedAt: r.AppliedAt}
	err := json.Unmarshal(r.Request, &out.SteerRunRequest)
	return out, err
}
func (s *SQL) CreateRunSteering(ctx context.Context, appID, runID string, req sdk.SteerRunRequest) (*sdk.RunSteeringCommand, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var result sdk.RunSteeringCommand
	err = s.retryTransaction(ctx, func(tx *gorm.DB) error {
		var run runRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("app_id = ? AND id = ?", appID, runID).Take(&run).Error; err != nil {
			return err
		}
		var old runSteeringRecord
		err := tx.Where("app_id = ? AND run_id = ? AND command_id = ?", appID, runID, req.CommandID).Take(&old).Error
		if err == nil {
			var previous sdk.SteerRunRequest
			if err = json.Unmarshal(old.Request, &previous); err != nil {
				return err
			}
			normalized, _ := json.Marshal(previous)
			if string(normalized) != string(raw) {
				return agentcore.ErrSteeringConflict
			}
			result, err = old.command()
			if agentcore.IsTerminalStatus(run.Status) && result.State == "accepted" {
				result.State = "not_applied"
			}
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if agentcore.IsTerminalStatus(run.Status) {
			return agentcore.ErrSteeringTerminal
		}
		var revision int64
		if err := tx.Model(&runSteeringRecord{}).Where("app_id = ? AND run_id = ?", appID, runID).Select("COALESCE(MAX(revision), 0)").Scan(&revision).Error; err != nil {
			return err
		}
		if revision != req.ExpectedRevision {
			return agentcore.ErrSteeringConflict
		}
		row := runSteeringRecord{AppID: appID, RunID: runID, CommandID: req.CommandID, Revision: revision + 1, Request: raw, State: "accepted", CreatedAt: time.Now().UTC()}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		result, err = row.command()
		return err
	})
	return &result, err
}
func (s *SQL) ListRunSteering(ctx context.Context, appID, runID string) ([]sdk.RunSteeringCommand, error) {
	var rows []runSteeringRecord
	if err := s.db.WithContext(ctx).Where("app_id = ? AND run_id = ?", appID, runID).Order("revision ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	run, err := s.GetRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	out := make([]sdk.RunSteeringCommand, 0, len(rows))
	for _, row := range rows {
		item, err := row.command()
		if err != nil {
			return nil, err
		}
		if run != nil && agentcore.IsTerminalStatus(run.Status) && item.State == "accepted" {
			item.State = "not_applied"
		}
		out = append(out, item)
	}
	return out, nil
}
func (s *SQL) ApplyRunSteering(ctx context.Context, appID, runID string, revision int64) error {
	return s.db.WithContext(ctx).Model(&runSteeringRecord{}).Where("app_id = ? AND run_id = ? AND revision <= ? AND state = ?", appID, runID, revision, "accepted").Updates(map[string]any{"state": "applied", "applied_at": time.Now().UTC()}).Error
}

func (s *SQL) RejectRunSteering(ctx context.Context, appID, runID string, revision int64) error {
	return s.db.WithContext(ctx).Model(&runSteeringRecord{}).Where("app_id = ? AND run_id = ? AND revision = ? AND state = ?", appID, runID, revision, "accepted").Update("state", "rejected").Error
}
