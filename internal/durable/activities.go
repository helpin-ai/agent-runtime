package durable

import (
	"context"
	"fmt"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
)

type AgentRunActivities struct {
	store  agentcore.Store
	engine *engine.Engine
}

func NewAgentRunActivities(store agentcore.Store, runner *engine.Engine) *AgentRunActivities {
	return &AgentRunActivities{store: store, engine: runner}
}

func (a *AgentRunActivities) PrepareRunActivity(ctx context.Context, appID, runID string) error {
	if a == nil || a.store == nil {
		return fmt.Errorf("agent run activities store is not configured")
	}
	run, err := a.store.GetRun(ctx, appID, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("run not found")
	}
	if agentcore.IsTerminalStatus(run.Status) {
		return nil
	}
	if run.Status == agentcore.RunStatusQueued {
		now := time.Now().UTC()
		run.Status = agentcore.RunStatusRunning
		run.PauseReason = agentcore.PauseReasonNone
		if run.StartedAt == nil {
			run.StartedAt = &now
		}
		return a.store.UpdateRun(ctx, run)
	}
	return nil
}

func (a *AgentRunActivities) ExecuteRunActivity(ctx context.Context, appID, runID string) (ExecuteRunResult, error) {
	if a == nil || a.engine == nil {
		return ExecuteRunResult{}, fmt.Errorf("agent run activities engine is not configured")
	}
	result, err := a.engine.ExecuteRunOnce(ctx, appID, runID)
	if err != nil {
		return ExecuteRunResult{}, err
	}
	if result == nil {
		return ExecuteRunResult{}, nil
	}
	return ExecuteRunResult{
		WaitForApproval: result.WaitForApproval,
		AwaitingInput:   result.AwaitingInput,
		AwaitingAuth:    result.AwaitingAuth,
	}, nil
}

func (a *AgentRunActivities) MarkRunFailedActivity(ctx context.Context, appID, runID, message string) error {
	if a == nil || a.store == nil {
		return fmt.Errorf("agent run activities store is not configured")
	}
	run, err := a.store.GetRun(ctx, appID, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return nil
	}
	now := time.Now().UTC()
	run.Status = agentcore.RunStatusFailed
	run.PauseReason = agentcore.PauseReasonNone
	run.ErrorMessage = message
	run.CompletedAt = &now
	return a.store.UpdateRun(ctx, run)
}
