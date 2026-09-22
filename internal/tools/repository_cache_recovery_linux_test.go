//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/sandbox"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

// Reuse a failed disposable Poetry checkout, not a live user's checkout. This
// exercises the actual run_command input/lease/environment/Landlock path.
func TestRepositoryCachePoetryRecoveryStaging(t *testing.T) {
	root := os.Getenv("AGENT_RUNTIME_CACHE_RECOVERY_ROOT")
	if root == "" {
		t.Skip("requires a failed disposable Poetry fixture")
	}
	if abi, err := sandbox.ABI(); err != nil || abi < 2 {
		t.Fatalf("Landlock required: %d %v", abi, err)
	}
	registry, callCtx := workspaceToolTestRegistry(t)
	t.Setenv(workspace.RepositoryCacheModeEnv, "repository")
	t.Setenv(workspace.EphemeralRootEnv, filepath.Join(t.TempDir(), "ephemeral"))
	workspace.RegisterRepositoryCache(root, "app", map[string]interface{}{"workspace_id": "smoke-workspace", "repository_id": "smoke-repository"})
	t.Cleanup(func() { workspace.CleanupToolState(root) })
	callCtx.Run.WorkspaceLease.RootPath = root
	binary := buildWorker(t)
	sandboxExecutable = func() (string, error) { return binary, nil }
	SetCommandSandbox(CommandSandboxLandlock)
	t.Cleanup(func() { sandboxExecutable = os.Executable; SetCommandSandbox(CommandSandboxNone) })
	started := time.Now()
	out, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"poetry","args":["install","--no-root","--no-interaction","--no-ansi"],"working_directory":"poetry","private_cache":true,"timeout_seconds":300}`))
	t.Logf("private recovery: %s %s", time.Since(started), out)
	if err != nil {
		t.Fatal(err)
	}
	// Normal commands must see the recovered private environment too.
	out, err = registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"poetry","args":["run","python","-c","import six; assert six.__version__ == '1.17.0'"],"working_directory":"poetry","timeout_seconds":60}`))
	if err != nil {
		t.Fatalf("recovered environment: %v %s", err, out)
	}
}
