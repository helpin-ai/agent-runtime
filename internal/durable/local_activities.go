package durable

import (
	"context"
	"fmt"

	"github.com/helpin-ai/agent-runtime/internal/workspace"
	"go.temporal.io/sdk/temporal"
)

type runLocker interface {
	SupportsRunLock() bool
	AcquireRunLock(context.Context, string, string, func()) (context.Context, func(), error)
}

func ValidateLocalWorkspaceStore(store interface{}) error {
	locker, ok := store.(runLocker)
	if !ok || !locker.SupportsRunLock() {
		return fmt.Errorf("ephemeral workspaces require a direct PostgreSQL connection with session advisory locks (not transaction-pooled PgBouncer)")
	}
	return nil
}

func (a *AgentRunActivities) localContext(ctx context.Context, appID, runID, session string) (context.Context, func(), error) {
	if !workspace.EphemeralWorkspaces() || session == "" {
		return ctx, func() {}, temporal.NewNonRetryableApplicationError("ephemeral workspace activity requires an ephemeral worker and a session", "WorkspaceConfiguration", nil)
	}
	if err := ValidateLocalWorkspaceStore(a.store); err != nil {
		return ctx, func() {}, err
	}
	ctx, release, err := a.store.(runLocker).AcquireRunLock(ctx, appID, runID, func() { recordActivityHeartbeatSafe(ctx, "waiting-for-database-run-lock") })
	return workspace.WithSession(ctx, session), release, err
}

func (a *AgentRunActivities) PrepareLocalRunActivity(ctx context.Context, appID, runID, session string) error {
	ctx, release, err := a.localContext(ctx, appID, runID, session)
	if err != nil {
		return err
	}
	defer release()
	return a.PrepareRunActivity(ctx, appID, runID)
}

func (a *AgentRunActivities) ExecuteLocalRunActivity(ctx context.Context, appID, runID, session string) (ExecuteRunResult, error) {
	ctx, release, err := a.localContext(ctx, appID, runID, session)
	if err != nil {
		return ExecuteRunResult{}, err
	}
	defer release()
	return a.ExecuteRunActivity(ctx, appID, runID)
}

func (a *AgentRunActivities) CleanupLocalWorkspaceActivity(ctx context.Context, appID, runID, session string) error {
	ctx, release, err := a.localContext(ctx, appID, runID, session)
	if err != nil {
		return err
	}
	defer release()
	return a.CleanupTerminalWorkspaceActivity(ctx, appID, runID)
}
