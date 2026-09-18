package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireRunLockBlocksSecondHolderUntilRelease(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", root)

	release, err := AcquireRunLock(context.Background(), "App/1", "run:1", nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	lockPath := filepath.Join(root, ".locks", "app-1-run-1.lock")
	holder, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if !strings.Contains(string(holder), "pid=") {
		t.Fatalf("lock file should record holder identity, got %q", holder)
	}

	// A second acquire must not succeed while the first is held. Bound it with
	// a short context so the test fails fast rather than waiting ten minutes.
	heartbeats := 0
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := AcquireRunLock(ctx, "App/1", "run:1", func() { heartbeats++ }); err == nil {
		t.Fatal("second acquire succeeded while the lock was held")
	} else if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("unexpected error while blocked: %v", err)
	}
	if heartbeats == 0 {
		t.Fatal("heartbeat was not invoked while waiting for the lock")
	}

	release()

	release2, err := AcquireRunLock(context.Background(), "App/1", "run:1", nil)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should survive release: %v", err)
	}
}

func TestAcquireRunLockDistinctRunsDoNotContend(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", t.TempDir())
	r1, err := AcquireRunLock(context.Background(), "app", "run-1", nil)
	if err != nil {
		t.Fatalf("acquire run-1: %v", err)
	}
	defer r1()
	r2, err := AcquireRunLock(context.Background(), "app", "run-2", nil)
	if err != nil {
		t.Fatalf("acquire run-2: %v", err)
	}
	r2()
}
