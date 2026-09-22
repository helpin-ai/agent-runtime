package durable

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

type AgentRunActivities struct {
	store  agentcore.Store
	engine *engine.Engine
	// Execution roles require per-run locking: retained workspaces use the
	// shared-volume lock; ephemeral session activities acquire PostgreSQL locks
	// before entering these handlers.
	lockWorkspaces bool
}

const workerInterruptedErrorType = "WorkerInterrupted"

func NewAgentRunActivities(store agentcore.Store, runner *engine.Engine) *AgentRunActivities {
	return &AgentRunActivities{
		store:          store,
		engine:         runner,
		lockWorkspaces: runner.ExecutesCommandCapableRuns(),
	}
}

// lockRunWorkspace takes the run's workspace fence when this worker serves an
// execution role. The returned release is always safe to call.
func (a *AgentRunActivities) lockRunWorkspace(ctx context.Context, appID, runID, stage string) (func(), error) {
	if a == nil || !a.lockWorkspaces {
		return func() {}, nil
	}
	if workspace.EphemeralWorkspaces() {
		if workspace.Session(ctx) == "" {
			return func() {}, temporal.NewNonRetryableApplicationError("drain retained-workspace runs before switching to ephemeral workers", "WorkspaceConfiguration", nil)
		}
		// Session activities already hold the PostgreSQL lock.
		return func() {}, nil
	}
	release, err := workspace.AcquireRunLock(ctx, appID, runID, func() {
		recordActivityHeartbeatSafe(ctx, stage+":waiting-for-workspace-lock")
	})
	if err != nil {
		return func() {}, err
	}
	return release, nil
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
	release, err := a.lockRunWorkspace(ctx, appID, runID, "preparing")
	if err != nil {
		return err
	}
	defer release()
	return a.engine.PrepareRunOnce(ctx, appID, runID)
}

func (a *AgentRunActivities) ExecuteRunActivity(ctx context.Context, appID, runID string) (ExecuteRunResult, error) {
	stopHeartbeat := startActivityHeartbeatLoop(ctx, "executing", 15*time.Second)
	defer stopHeartbeat()

	if a == nil || a.engine == nil {
		return ExecuteRunResult{}, fmt.Errorf("agent run activities engine is not configured")
	}
	release, err := a.lockRunWorkspace(ctx, appID, runID, "executing")
	if err != nil {
		return ExecuteRunResult{}, err
	}
	defer release()
	result, err := a.engine.ExecuteRunOnce(ctx, appID, runID)
	if err != nil {
		return ExecuteRunResult{}, durableExecutionError(ctx, err)
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

func durableExecutionError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return temporal.NewApplicationErrorWithCause(
			"agent run execution was interrupted",
			workerInterruptedErrorType,
			err,
		)
	}
	// The engine already records genuine agent/provider/tool failures. Marking
	// them non-retryable preserves existing semantics while allowing the retry
	// policy to be reserved for infrastructure interruption.
	return temporal.NewNonRetryableApplicationError(
		err.Error(),
		"RunExecutionFailed",
		err,
	)
}

func (a *AgentRunActivities) MarkRunFailedActivity(ctx context.Context, appID, runID, message string) error {
	if a == nil || a.store == nil {
		return fmt.Errorf("agent run activities store is not configured")
	}
	if workspace.EphemeralWorkspaces() {
		if err := ValidateLocalWorkspaceStore(a.store); err != nil {
			return err
		}
		lockedCtx, release, err := a.store.(runLocker).AcquireRunLock(ctx, appID, runID, nil)
		if err != nil {
			return err
		}
		defer release()
		ctx = lockedCtx
	}
	run, err := a.store.GetRun(ctx, appID, runID)
	if err != nil {
		return err
	}
	if run == nil || agentcore.IsTerminalStatus(run.Status) {
		return nil
	}
	now := time.Now().UTC()
	run.Status = agentcore.RunStatusFailed
	run.PauseReason = agentcore.PauseReasonNone
	run.ErrorMessage = message
	run.CompletedAt = &now
	return a.store.UpdateRun(ctx, run)
}

func (a *AgentRunActivities) CleanupTerminalWorkspaceActivity(ctx context.Context, appID, runID string) error {
	if a == nil || a.engine == nil {
		return fmt.Errorf("agent run activities engine is not configured")
	}
	release, err := a.lockRunWorkspace(ctx, appID, runID, "cleaning")
	if err != nil {
		return err
	}
	defer release()
	return a.engine.CleanupTerminalWorkspace(ctx, appID, runID)
}
