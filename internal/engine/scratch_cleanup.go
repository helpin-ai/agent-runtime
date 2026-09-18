package engine

import (
	"context"
	"fmt"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// CleanupTerminalWorkspace executes on the run's original worker queue so
// paused cancellations also reach the retained execution volume.
func (e *Engine) CleanupTerminalWorkspace(ctx context.Context, appID, runID string) error {
	run, err := e.cfg.Store.GetRun(ctx, appID, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return nil
	}
	if !agentcore.IsTerminalStatus(run.Status) {
		return fmt.Errorf("awaiting terminal run state before workspace cleanup")
	}
	if err := e.captureInterruptedEffects(ctx, run); err != nil {
		return err
	}
	return e.cleanupWorkspaceOnce(ctx, run, run.Status, true)
}
