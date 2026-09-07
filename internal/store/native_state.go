package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type nativeStateRecord struct {
	AppID     string    `gorm:"primaryKey"`
	RunID     string    `gorm:"primaryKey"`
	Version   int64     `gorm:"not null"`
	Payload   jsonBytes `gorm:"type:json;not null"`
	UpdatedAt time.Time
}

func (nativeStateRecord) TableName() string { return "native_run_states" }

type nativeJournalRecord struct {
	AppID     string    `gorm:"primaryKey"`
	RunID     string    `gorm:"primaryKey"`
	Version   int64     `gorm:"primaryKey"`
	Position  int       `gorm:"primaryKey"`
	Payload   jsonBytes `gorm:"type:json;not null"`
	CreatedAt time.Time
}

func (nativeJournalRecord) TableName() string { return "native_run_journal" }

// LoadNativeState reads the latest private checkpoint for this app and run.
func (s *SQL) LoadNativeState(ctx context.Context, appID, runID string) (*agentcore.NativeState, error) {
	var row nativeStateRecord
	err := s.db.WithContext(ctx).Where("app_id = ? AND run_id = ?", appID, runID).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &agentcore.NativeState{AppID: appID, RunID: runID, Version: row.Version, Payload: append(json.RawMessage(nil), row.Payload...)}, nil
}

// SaveNativeState uses the supplied version as a compare-and-swap condition.
func (s *SQL) SaveNativeState(ctx context.Context, state *agentcore.NativeState, records []json.RawMessage) error {
	if err := validateNativeState(state, records); err != nil {
		return err
	}
	next := state.Version + 1
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := nativeStateRecord{AppID: state.AppID, RunID: state.RunID, Version: next, Payload: jsonBytes(state.Payload), UpdatedAt: time.Now().UTC()}
		var result *gorm.DB
		if state.Version == 0 {
			result = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		} else {
			result = tx.Model(&nativeStateRecord{}).Where("app_id = ? AND run_id = ? AND version = ?", state.AppID, state.RunID, state.Version).
				Updates(map[string]any{"version": next, "payload": row.Payload, "updated_at": row.UpdatedAt})
		}
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return agentcore.ErrNativeStateConflict
		}
		for i, payload := range records {
			if err := tx.Create(&nativeJournalRecord{AppID: state.AppID, RunID: state.RunID, Version: next, Position: i, Payload: jsonBytes(payload), CreatedAt: row.UpdatedAt}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		state.Version = next
	}
	return err
}

func validateNativeState(state *agentcore.NativeState, records []json.RawMessage) error {
	if state == nil || state.AppID == "" || state.RunID == "" || state.Version < 0 || !json.Valid(state.Payload) {
		return fmt.Errorf("valid native checkpoint and app/run identity are required")
	}
	for _, record := range records {
		if !json.Valid(record) {
			return fmt.Errorf("native journal record must be JSON")
		}
	}
	return nil
}

// LoadNativeState reads an isolated copy of the checkpoint.
func (m *Memory) LoadNativeState(_ context.Context, appID, runID string) (*agentcore.NativeState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, ok := m.nativeStates[key(appID, runID)]
	if !ok {
		return nil, nil
	}
	state.Payload = append(json.RawMessage(nil), state.Payload...)
	return &state, nil
}

// SaveNativeState atomically archives records and advances the checkpoint version.
func (m *Memory) SaveNativeState(_ context.Context, state *agentcore.NativeState, records []json.RawMessage) error {
	if err := validateNativeState(state, records); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(state.AppID, state.RunID)
	if m.nativeStates[k].Version != state.Version {
		return agentcore.ErrNativeStateConflict
	}
	next := *state
	next.Version++
	next.Payload = append(json.RawMessage(nil), state.Payload...)
	m.nativeStates[k] = next
	for _, record := range records {
		m.nativeJournal[k] = append(m.nativeJournal[k], append(json.RawMessage(nil), record...))
	}
	state.Version = next.Version
	return nil
}
