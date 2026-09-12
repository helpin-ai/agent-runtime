package store

import (
	"context"
	"errors"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"gorm.io/gorm"
)

func cloneModelCredential(c agentcore.RunModelCredential) agentcore.RunModelCredential {
	c.EncryptedCredential = append([]byte(nil), c.EncryptedCredential...)
	return c
}

func (m *Memory) GetRunModelCredential(_ context.Context, appID, runID string) (*agentcore.RunModelCredential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.modelCredentials[key(appID, runID)]
	if !ok {
		return nil, nil
	}
	c = cloneModelCredential(c)
	return &c, nil
}
func (m *Memory) ReplaceRunModelCredential(_ context.Context, c *agentcore.RunModelCredential, version int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(c.AppID, c.RunID)
	old, ok := m.modelCredentials[k]
	run := m.runs[k]
	if !ok || old.Version != version || run == nil || agentcore.IsTerminalStatus(run.Status) {
		return false, nil
	}
	next := cloneModelCredential(*c)
	next.Version = version + 1
	m.modelCredentials[k] = next
	return true, nil
}
func (m *Memory) ClearRunModelCredential(_ context.Context, appID, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(appID, runID)
	c, ok := m.modelCredentials[k]
	if ok {
		c.EncryptedCredential = nil
		c.Revoked = true
		c.Version++
		m.modelCredentials[k] = c
	}
	return nil
}

func (s *SQL) GetRunModelCredential(ctx context.Context, appID, runID string) (*agentcore.RunModelCredential, error) {
	var c agentcore.RunModelCredential
	err := s.db.WithContext(ctx).Where("app_id = ? AND run_id = ?", appID, runID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &c, err
}
func (s *SQL) ReplaceRunModelCredential(ctx context.Context, c *agentcore.RunModelCredential, version int64) (bool, error) {
	result := s.db.WithContext(ctx).Model(&agentcore.RunModelCredential{}).
		Where("app_id = ? AND run_id = ? AND version = ?", c.AppID, c.RunID, version).
		Where("EXISTS (SELECT 1 FROM agent_runs WHERE agent_runs.app_id = ? AND agent_runs.id = ? AND agent_runs.status IN ?)", c.AppID, c.RunID, []string{agentcore.RunStatusQueued, agentcore.RunStatusRunning, agentcore.RunStatusPaused}).
		Updates(map[string]any{"encrypted_credential": c.EncryptedCredential, "revoked": c.Revoked, "version": version + 1})
	return result.RowsAffected == 1, result.Error
}
func (s *SQL) ClearRunModelCredential(ctx context.Context, appID, runID string) error {
	return s.db.WithContext(ctx).Model(&agentcore.RunModelCredential{}).Where("app_id = ? AND run_id = ?", appID, runID).
		Updates(map[string]any{"encrypted_credential": nil, "revoked": true, "version": gorm.Expr("version + 1")}).Error
}
