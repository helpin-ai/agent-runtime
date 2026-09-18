package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	runLockRetryInterval = 2 * time.Second
	runLockAcquireLimit  = 10 * time.Minute
)

// AcquireRunLock takes an exclusive advisory lock fencing a run's workspace so
// that two execution workers never write the same checkout concurrently. The
// lock lives on the workspace volume, so on a cluster-wide filesystem it is
// cluster-wide as well. While another holder keeps the lock the call waits,
// invoking heartbeat on every retry, and gives up after ten minutes. The
// returned release drops the lock; the lock file itself is never removed.
func AcquireRunLock(ctx context.Context, appID, runID string, heartbeat func()) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dir := filepath.Join(WorkspaceRoot(), ".locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create run lock directory: %w", err)
	}
	path := filepath.Join(dir, sanitizePathComponent(appID)+"-"+sanitizePathComponent(runID)+".lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open run lock %s: %w", path, err)
	}

	deadline := time.Now().Add(runLockAcquireLimit)
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = file.Close()
			return nil, fmt.Errorf("lock run workspace %s: %w", path, err)
		}
		if heartbeat != nil {
			heartbeat()
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("another worker still holds this run's workspace lock (%s) after %s", path, runLockAcquireLimit)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("waiting for run workspace lock %s: %w", path, ctx.Err())
		case <-time.After(runLockRetryInterval):
		}
	}

	// Holder identity is diagnostic only; the flock is the fence.
	hostname, _ := os.Hostname()
	_ = file.Truncate(0)
	_, _ = file.WriteAt([]byte(fmt.Sprintf("%s pid=%d at=%s\n", hostname, os.Getpid(), time.Now().UTC().Format(time.RFC3339))), 0)

	return func() {
		// Closing the descriptor drops the flock. The file stays in place so a
		// stale holder line remains readable for diagnostics.
		_ = file.Close()
	}, nil
}

// WorkspaceRoot returns the directory holding execution workspaces on this
// worker's writable volume.
func WorkspaceRoot() string {
	if root := os.Getenv("AGENT_RUNTIME_WORKSPACE_ROOT"); root != "" {
		return root
	}
	return "/tmp/agent-runtime-workspaces"
}
