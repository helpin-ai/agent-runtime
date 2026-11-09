package runtime

import (
	"context"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// The run's metadata selects this check, but authority comes only from the store.
func nativeOwnedWorkItems(ctx context.Context, x *ExecutionContext) ([]string, bool, error) {
	if x.Run.Input.Metadata["work_attempt_id"] == nil {
		return nil, false, nil
	}
	store, ok := x.Store.(agentcore.WorkJobStore)
	if !ok {
		return nil, true, fmt.Errorf("cannot verify worker assignment")
	}
	attempt, err := store.GetWorkAttemptByRun(ctx, x.AppID, x.Run.ID)
	if err != nil {
		return nil, true, err
	}
	if attempt == nil || attempt.State != agentcore.WorkAttemptStateOpen {
		return nil, true, nil
	}
	generation, ok := agentcore.ExecutionGenerationFromContext(ctx)
	if !ok || generation != attempt.Generation {
		return nil, true, fmt.Errorf("worker execution authority changed; stop this attempt")
	}
	job, err := store.GetWorkJob(ctx, x.AppID, attempt.JobID)
	if err != nil {
		return nil, true, err
	}
	// Pausing stops dispatch; existing batches are allowed to drain.
	if agentcore.IsTerminalWorkJobState(job.State) {
		return nil, true, fmt.Errorf("job is %s; no worker actions may start", job.State)
	}
	items, err := store.GetWorkItems(ctx, x.AppID, attempt.JobID, attempt.ItemIDs)
	if err != nil {
		return nil, true, err
	}
	var owned []string
	for _, item := range items {
		if item.AttemptID == attempt.ID && item.State == agentcore.WorkItemStateRunning {
			owned = append(owned, item.ID)
		}
	}
	return owned, true, nil
}

func nativeWorkAllowsTool(ctx context.Context, x *ExecutionContext, name string) error {
	if name == nativeToolFinishTurn || name == nativeToolReadResult || name == "submit_item_result" {
		return nil
	}
	owned, worker, err := nativeOwnedWorkItems(ctx, x)
	if err != nil {
		return err
	}
	if worker && len(owned) == 0 {
		return fmt.Errorf("this attempt has no remaining assigned items; do not perform further actions, finish the turn")
	}
	return nil
}
