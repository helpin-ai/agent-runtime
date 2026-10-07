//go:build darwin

package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/sandbox"
)

func TestRunCommandConfinedBySeatbelt(t *testing.T) {
	if err := sandbox.SeatbeltAvailable(); err != nil {
		t.Skip(err)
	}
	t.Setenv("AGENT_RUNTIME_EPHEMERAL_ROOT", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_test"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	SetCommandSandbox(CommandSandboxSeatbelt)
	t.Cleanup(func() { SetCommandSandbox(CommandSandboxNone) })
	registry, call := workspaceToolTestRegistry(t)
	root := call.Run.WorkspaceLease.RootPath
	sibling := filepath.Join(filepath.Dir(root), "sibling-run")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(input string) (string, error) {
		result, err := registry.Execute(context.Background(), call, "run_command", json.RawMessage(input))
		return string(result), err
	}

	if _, err := run(`{"program":"mkdir","args":["made"]}`); err != nil {
		t.Fatalf("write inside the run root failed: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "made")); err != nil || !info.IsDir() {
		t.Fatalf("directory not created inside the run root: %v", err)
	}
	if _, err := run(`{"program":"mkdir","args":["` + filepath.Join(sibling, "escape") + `"]}`); err == nil {
		t.Fatal("write to a sibling run was not denied")
	}
	if _, err := os.Stat(filepath.Join(sibling, "escape")); err == nil {
		t.Fatal("sibling directory exists despite the sandbox")
	}
	outside := filepath.Join(os.TempDir(), "seatbelt-outside-"+filepath.Base(root))
	_, err := run(`{"program":"mkdir","args":["` + outside + `"]}`)
	_ = os.Remove(outside)
	if err == nil {
		t.Fatal("write outside the run root was not denied")
	}
	if !strings.Contains(err.Error(), sandboxDeniedNote) {
		t.Fatalf("denial should carry the workspace note: %v", err)
	}
	if _, err := run(`{"program":"cat","args":["` + filepath.Join(home, ".ssh", "id_test") + `"]}`); err == nil {
		t.Fatal("reading ~/.ssh was not denied")
	}
	if _, err := run(`{"program":"mkdir","args":["` + filepath.Join(home, "home-escape") + `"]}`); err == nil {
		t.Fatal("write to the user's real home was not denied")
	}
	if _, err := exec.LookPath("python3"); err == nil {
		result, err := run(`{"program":"python3","args":["-c","import os; print(1); print(os.environ['HOME'])"]}`)
		if err != nil || !strings.Contains(result, filepath.Join(root, ".agent-runtime")) {
			t.Fatalf("python3 under seatbelt: %v %s", err, result)
		}
	}
	if _, err := exec.LookPath("git"); err == nil {
		if _, err := run(`{"program":"git","args":["init","-q","repo"]}`); err != nil {
			t.Fatalf("git under seatbelt: %v", err)
		}
	}
}

func TestSeatbeltTimeoutKillsProcessGroup(t *testing.T) {
	if err := sandbox.SeatbeltAvailable(); err != nil {
		t.Skip(err)
	}
	t.Setenv("AGENT_RUNTIME_EPHEMERAL_ROOT", "")
	SetCommandSandbox(CommandSandboxSeatbelt)
	t.Cleanup(func() { SetCommandSandbox(CommandSandboxNone) })
	registry, call := workspaceToolTestRegistry(t)
	started := time.Now()
	_, err := registry.Execute(context.Background(), call, "run_command", json.RawMessage(`{"program":"python3","args":["-c","import subprocess, time; subprocess.Popen(['sleep', '30']); time.sleep(30)"],"timeout_seconds":2}`))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "tim") {
		t.Fatalf("a command over its timeout should fail with a timeout: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("timeout did not stop the sandboxed process group (took %s)", elapsed)
	}
}
