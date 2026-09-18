package durable

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestAgentRunActivitiesFenceWorkspaceInExecutionRole(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", t.TempDir())
	mem := store.NewMemory()

	execution := NewAgentRunActivities(mem, engine.New(engine.Config{Store: mem, CodingWorker: true}))
	shared := NewAgentRunActivities(mem, engine.New(engine.Config{Store: mem}))

	release, err := workspace.AcquireRunLock(context.Background(), "app-a", "run-1", nil)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = execution.CleanupTerminalWorkspaceActivity(ctx, "app-a", "run-1")
	if err == nil || !strings.Contains(err.Error(), "workspace lock") {
		t.Fatalf("execution role should wait on the held fence, got err=%v", err)
	}

	// Shared (non-execution) roles never touch the workspace volume and so
	// never contend for the fence.
	if err := shared.CleanupTerminalWorkspaceActivity(ctx, "app-a", "run-1"); err != nil {
		t.Fatalf("shared role cleanup: %v", err)
	}

	release()
	if err := execution.CleanupTerminalWorkspaceActivity(context.Background(), "app-a", "run-1"); err != nil {
		t.Fatalf("execution role cleanup after release: %v", err)
	}
}
