package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestAnalysisScratchRetainedUntilTerminal(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", t.TempDir())
	t.Setenv(workspace.EphemeralRootEnv, filepath.Join(t.TempDir(), "agent-runtime-ephemeral"))
	mem := store.NewMemory()
	agent := agentcore.Agent{ID: "a", AppID: "host", AllowedTools: []string{"run_python"}}
	run := &agentcore.AgentRun{ID: "run", AppID: "host", AgentID: agent.ID}
	ctx := context.Background()
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	engine := New(Config{Store: mem})
	lease, err := engine.ensureWorkspace(ctx, &agent, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(lease.RootPath, "result.csv")
	if err := os.WriteFile(file, []byte("x\n1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state, ephemeral, err := workspace.ToolStateRoot(lease.RootPath)
	if err != nil || !ephemeral {
		t.Fatal("ephemeral state unavailable", state, ephemeral, err)
	}
	stateFile := filepath.Join(state, "cache")
	if err := os.WriteFile(stateFile, []byte("cache"), 0600); err != nil {
		t.Fatal(err)
	}
	engine.cleanupWorkspace(ctx, run, "paused", false)
	got, err := engine.ensureWorkspace(ctx, &agent, run, nil)
	if err != nil || got.ID != lease.ID {
		t.Fatal("lease changed", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("paused workspace lost", err)
	}
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatal("paused ephemeral state lost", err)
	}
	engine.cleanupWorkspace(ctx, run, "completed", true)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("terminal workspace survived")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("terminal ephemeral state survived")
	}
	if _, err := engine.ensureWorkspace(ctx, &agent, run, nil); err == nil {
		t.Fatal("lost state silently recreated")
	}
	successor, err := workspace.NewScratch(run.AppID, "successor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(successor.RootPath, "result.csv")); !os.IsNotExist(err) {
		t.Fatal("successor inherited files")
	}
}

func TestTerminalCleanupOnExecutionWorkerAfterAPICancellation(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", t.TempDir())
	ctx := context.Background()
	mem := store.NewMemory()
	lease, err := workspace.NewScratch("app", "run")
	if err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{AppID: "app", ID: "run", Status: agentcore.RunStatusPaused, WorkspaceLease: lease}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	e := New(Config{Store: mem})
	if err := e.CleanupTerminalWorkspace(ctx, "app", "run"); err == nil {
		t.Fatal("active run cleaned")
	}
	run.Status = agentcore.RunStatusCancelled
	if err := mem.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := e.CleanupTerminalWorkspace(ctx, "app", "run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lease.RootPath); !os.IsNotExist(err) {
		t.Fatal("terminal scratch survived")
	}
	if err := e.CleanupTerminalWorkspace(ctx, "app", "run"); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
}

func TestRepositoryTerminalCleanupDoesNotSweepAnalysisPath(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", t.TempDir())
	lease, err := workspace.NewScratch("app", "run")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.CleanupScratch("app", "run") })
	marker := filepath.Join(lease.RootPath, "keep")
	if err := os.WriteFile(marker, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{
		AppID: "app",
		ID:    "run",
		WorkspaceLease: &agentcore.WorkspaceLease{
			Provider: "git",
			RootPath: "/repository/workspace",
		},
	}
	New(Config{}).cleanupWorkspace(context.Background(), run, "completed", true)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("repository cleanup swept analysis path: %v", err)
	}
}
