package store

import (
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"time"
)

func cloneSteering(c sdk.RunSteeringCommand) sdk.RunSteeringCommand {
	c.DeniedTools = append([]string(nil), c.DeniedTools...)
	if c.AppliedAt != nil {
		v := *c.AppliedAt
		c.AppliedAt = &v
	}
	return c
}
func (m *Memory) CreateRunSteering(_ context.Context, appID, runID string, req sdk.SteerRunRequest) (*sdk.RunSteeringCommand, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(appID, runID)
	run := m.runs[k]
	if run == nil {
		return nil, fmt.Errorf("run not found")
	}
	commands := m.steering[k]
	raw, _ := json.Marshal(req)
	for _, command := range commands {
		if command.CommandID == req.CommandID {
			previous, _ := json.Marshal(command.SteerRunRequest)
			if string(raw) != string(previous) {
				return nil, agentcore.ErrSteeringConflict
			}
			out := cloneSteering(command)
			if agentcore.IsTerminalStatus(run.Status) && out.State == "accepted" {
				out.State = "not_applied"
			}
			return &out, nil
		}
	}
	if agentcore.IsTerminalStatus(run.Status) {
		return nil, agentcore.ErrSteeringTerminal
	}
	if int64(len(commands)) != req.ExpectedRevision {
		return nil, agentcore.ErrSteeringConflict
	}
	out := sdk.RunSteeringCommand{SteerRunRequest: req, RunID: runID, Revision: int64(len(commands) + 1), State: "accepted", CreatedAt: time.Now().UTC()}
	m.steering[k] = append(commands, cloneSteering(out))
	return &out, nil
}
func (m *Memory) ListRunSteering(_ context.Context, appID, runID string) ([]sdk.RunSteeringCommand, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []sdk.RunSteeringCommand{}
	for _, c := range m.steering[key(appID, runID)] {
		item := cloneSteering(c)
		if run := m.runs[key(appID, runID)]; run != nil && agentcore.IsTerminalStatus(run.Status) && item.State == "accepted" {
			item.State = "not_applied"
		}
		out = append(out, item)
	}
	return out, nil
}
func (m *Memory) ApplyRunSteering(_ context.Context, appID, runID string, revision int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(appID, runID)
	for i := range m.steering[k] {
		c := &m.steering[k][i]
		if c.Revision <= revision && c.State == "accepted" {
			now := time.Now().UTC()
			c.State = "applied"
			c.AppliedAt = &now
		}
	}
	return nil
}

func (m *Memory) RejectRunSteering(_ context.Context, appID, runID string, revision int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.steering[key(appID, runID)] {
		c := &m.steering[key(appID, runID)][i]
		if c.Revision == revision && c.State == "accepted" {
			c.State = "rejected"
		}
	}
	return nil
}
