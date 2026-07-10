package durable

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"go.temporal.io/sdk/activity"
)

type AgentRunActivities struct {
	store  agentcore.Store
	engine *engine.Engine
}

func NewAgentRunActivities(store agentcore.Store, runner *engine.Engine) *AgentRunActivities {
	return &AgentRunActivities{store: store, engine: runner}
}

func recordActivityHeartbeatSafe(ctx context.Context, details ...interface{}) {
	defer func() {
		if recover() != nil {
			// Unit tests may call activities without a Temporal activity context.
		}
	}()
	activity.RecordHeartbeat(ctx, details...)
}

func startActivityHeartbeatLoop(ctx context.Context, initialStage string, interval time.Duration) func() {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if initialStage == "" {
		initialStage = "running"
	}
	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		recordActivityHeartbeatSafe(ctx, initialStage)
		for {
			select {
			case <-ticker.C:
				recordActivityHeartbeatSafe(ctx, initialStage)
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() {
		stopOnce.Do(func() {
			close(done)
		})
	}
}

func (a *AgentRunActivities) PrepareRunActivity(ctx context.Context, appID, runID string) error {
	stopHeartbeat := startActivityHeartbeatLoop(ctx, "preparing", 15*time.Second)
	defer stopHeartbeat()

	if a == nil || a.engine == nil {
		return fmt.Errorf("agent run activities engine is not configured")
	}
	return a.engine.PrepareRunOnce(ctx, appID, runID)
}

func (a *AgentRunActivities) ExecuteRunActivity(ctx context.Context, appID, runID string) (ExecuteRunResult, error) {
	stopHeartbeat := startActivityHeartbeatLoop(ctx, "executing", 15*time.Second)
	defer stopHeartbeat()

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
