package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// UpdateRunOutputSummary cannot change a run's cancellation or completion state.
func (s *SQL) UpdateRunOutputSummary(ctx context.Context, appID, runID string, summary json.RawMessage) error {
	if !json.Valid(summary) {
		return fmt.Errorf("run output summary must be JSON")
	}
	result := s.db.WithContext(ctx).Model(&runRecord{}).Where("app_id = ? AND id = ?", appID, runID).Update("output_summary", jsonBytes(summary))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("run not found")
	}
	return nil
}

// UpdateRunOutputSummary changes only output under the store lock.
func (m *Memory) UpdateRunOutputSummary(ctx context.Context, appID, runID string, summary json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !json.Valid(summary) {
		return fmt.Errorf("run output summary must be JSON")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.runs[key(appID, runID)]
	if run == nil {
		return fmt.Errorf("run not found")
	}
	run.OutputSummary = append(json.RawMessage(nil), summary...)
	return nil
}
