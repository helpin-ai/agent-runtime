package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/sandbox/wsl"
	"golang.org/x/sys/windows"
)

func TestWSLBridgeCommandsPythonAndCancellation(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_WSL_E2E") != "1" {
		t.Skip("requires owned WSL2 fixture")
	}
	if err := ConfigureWSLCommandSandboxFromEnv(); err != nil {
		t.Fatal(err)
	}
	defer SetCommandSandbox(CommandSandboxNone)
	root := t.TempDir()
	registry := NewRegistry()
	RegisterWorkspaceTools(registry)
	agent := &agentcore.Agent{ID: "test", AppID: "test", AllowedTools: []string{"run_command", "run_python"}, ExecutionConfig: json.RawMessage(`{"workspace":{"mode":"host_prepared","access":"read_write"}}`)}
	call := CallContext{AppID: "test", RunID: "test", Agent: agent, Run: &agentcore.AgentRun{ID: "test", AppID: "test", WorkspaceLease: &agentcore.WorkspaceLease{ID: "test", RootPath: root, Provider: "host_prepared"}}}
	execute := func(tool string, input any) (json.RawMessage, error) {
		raw, _ := json.Marshal(input)
		return registry.Execute(t.Context(), call, tool, raw)
	}
	raw, err := execute("run_command", map[string]any{"program": "echo", "args": []string{"literal ; & $(data)"}})
	var literal string
	if err != nil || json.Unmarshal(raw, &literal) != nil || strings.TrimSpace(literal) != "literal ; & $(data)" {
		t.Fatalf("literal command: %s %v", raw, err)
	}
	raw, err = execute("run_python", map[string]any{"source": "from pathlib import Path\nprint('python-ready')\nPath('result.txt').write_text('saved')"})
	if err != nil || !strings.Contains(string(raw), "python-ready") {
		t.Fatalf("Python: %s %v", raw, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "result.txt")); err != nil || string(data) != "saved" {
		t.Fatalf("Windows output file: %q %v", data, err)
	}
	outside := filepath.Join(t.TempDir(), "fake-credential")
	if err := os.WriteFile(outside, []byte("never expose this fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	outsideLinux, err := wsl.NativePath(outside)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := execute("run_command", map[string]any{"program": "cat", "args": []string{outsideLinux}}); err == nil || strings.Contains(string(raw), "never expose") {
		t.Fatalf("outside read: %s %v", raw, err)
	}
	if _, err := execute("run_command", map[string]any{"program": "python3", "args": []string{"-c", "import time; time.sleep(60)"}, "timeout_seconds": 1}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("command timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := "import subprocess,time\nfrom pathlib import Path\np=subprocess.Popen(['python3','-c','import time; time.sleep(120)'])\nPath('child.pid').write_text(str(p.pid))\ntime.sleep(120)"
	input, _ := json.Marshal(map[string]any{"source": source})
	done := make(chan error, 1)
	go func() { _, err := registry.Execute(ctx, call, "run_python", input); done <- err }()
	deadline := time.Now().Add(30 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(root, "child.pid")); err == nil {
			pid, _ = strconv.Atoi(string(data))
			if pid > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("Linux descendant did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled Python succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WSL cancellation did not finish")
	}
	waitWSLProcessGone(t, pid)
}

func TestWSLBridgeRuntimeDeath(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_WSL_E2E") != "1" {
		t.Skip("requires owned WSL2 fixture")
	}
	if root := os.Getenv("WSL_TEST_CHILD_ROOT"); root != "" {
		config := wsl.Config{Distribution: os.Getenv("AGENT_RUNTIME_WSL_DISTRIBUTION"), Helper: os.Getenv("AGENT_RUNTIME_WSL_HELPER"), ProbeRoot: os.Getenv("AGENT_RUNTIME_WSL_PROBE_ROOT")}
		linuxRoot, err := wsl.NativePath(root)
		if err != nil {
			t.Fatal(err)
		}
		input := json.RawMessage(`{"program":"python3","args":["-c","import os,time; from pathlib import Path; Path('orphan.pid').write_text(str(os.getpid())); time.sleep(120)"]}`)
		_, err = config.Run(t.Context(), "execute", wsl.Request{Tool: "run_command", Input: input, Root: linuxRoot, AppID: "test", AgentID: "test", RunID: "test", LeaseID: "test"})
		t.Fatalf("parent-death fixture unexpectedly returned: %v", err)
	}
	root := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestWSLBridgeRuntimeDeath$", "-test.timeout=3m")
	child.Env = append(os.Environ(), "WSL_TEST_CHILD_ROOT="+root)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })
	deadline := time.Now().Add(30 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(root, "orphan.pid")); err == nil {
			pid, _ = strconv.Atoi(string(data))
			if pid > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("parent-death command did not start")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	waitWSLProcessGone(t, pid)
}

func waitWSLProcessGone(t *testing.T, pid int) {
	t.Helper()
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		out, err := exec.CommandContext(ctx, filepath.Join(system, "wsl.exe"), "-d", os.Getenv("AGENT_RUNTIME_WSL_DISTRIBUTION"), "--exec", "sh", "-c", `if kill -0 "$1" 2>/dev/null; then echo alive; else echo gone; fi`, "sh", strconv.Itoa(pid)).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("could not check Linux process: %s %v", out, err)
		}
		if strings.TrimSpace(string(out)) == "gone" {
			break
		}
		if strings.TrimSpace(string(out)) != "alive" {
			t.Fatalf("unexpected Linux process check: %q", out)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Linux command %d survived Windows runtime death", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
