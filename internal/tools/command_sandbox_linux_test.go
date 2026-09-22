//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/sandbox"
)

// buildWorker compiles the worker binary that carries the landlock-exec shim.
func buildWorker(t *testing.T) string {
	t.Helper()
	if binary := os.Getenv("AGENT_RUNTIME_TEST_WORKER_BINARY"); binary != "" {
		return binary
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not available")
	}
	binary := filepath.Join(t.TempDir(), "agent-runtime-worker")
	cmd := exec.Command(goTool, "build", "-o", binary, "../../cmd/agent-runtime-worker")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build worker: %v\n%s", err, output)
	}
	return binary
}

func TestRunCommandConfinedByLandlock(t *testing.T) {
	abi, err := sandbox.ABI()
	if err != nil {
		t.Fatal(err)
	}
	if abi < 2 {
		t.Skipf("landlock abi %d < 2", abi)
	}
	binary := buildWorker(t)
	sandboxExecutable = func() (string, error) { return binary, nil }
	SetCommandSandbox(CommandSandboxLandlock)
	t.Cleanup(func() {
		sandboxExecutable = os.Executable
		SetCommandSandbox(CommandSandboxNone)
	})
	registry, call := workspaceToolTestRegistry(t)
	root := call.Run.WorkspaceLease.RootPath
	sibling := filepath.Join(filepath.Dir(root), "sibling-run")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(sibling, "secret.txt")
	if err := os.WriteFile(secret, []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(input string) (string, error) {
		result, err := registry.Execute(context.Background(), call, "run_command", json.RawMessage(input))
		return string(result), err
	}
	_, err = run(`{"program":"cat","args":["` + secret + `"]}`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "permission denied") || !strings.Contains(err.Error(), sandboxDeniedNote) {
		t.Fatalf("sibling read was not denied with the workspace note: %v", err)
	}
	if _, err := run(`{"program":"cp","args":["/etc/hostname","copied.txt"]}`); err != nil {
		t.Fatalf("write inside run root failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "copied.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := run(`{"program":"mkdir","args":["` + filepath.Join(sibling, "made") + `"]}`); err == nil {
		t.Fatal("sibling directory creation was not denied")
	}
	if _, err := exec.LookPath("python3"); err == nil {
		result, err := run(`{"program":"python3","args":["-c","import os; print(1); print(os.environ['HOME'])"]}`)
		if err != nil || !strings.HasPrefix(strings.Trim(result, `"`), "1") || !strings.Contains(result, filepath.Join(root, ".agent-runtime")) {
			t.Fatalf("python3 under landlock: %v %s", err, result)
		}
	}
}
