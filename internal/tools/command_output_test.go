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
	"unicode/utf8"
)

func TestCommandOutputRetainsBoundedUTF8Tail(t *testing.T) {
	b := &commandOutput{}
	data := []byte(strings.Repeat("€", commandOutputLimit) + "\nfinal diagnostic\n")
	for i := 0; i < len(data); i += 7 {
		end := min(i+7, len(data))
		if _, err := b.Write(data[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	if len(b.tail) > commandOutputLimit || !utf8.ValidString(b.String()) || !strings.HasSuffix(b.String(), "final diagnostic\n") || !strings.Contains(b.String(), "truncated") {
		t.Fatalf("invalid bounded output: %d bytes", len(b.tail))
	}
}

func TestCommandFailureRetainsDiagnostic(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	_, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"cat","args":["does-not-exist"]}`))
	if err == nil || !strings.Contains(err.Error(), "does-not-exist") || !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("failure=%v", err)
	}
}

func TestCommandTimeoutAndEmptySuccess(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	registry, callCtx := workspaceToolTestRegistry(t)
	_, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"python3","args":["-c","import time; print('before timeout',flush=True); time.sleep(10)"],"timeout_seconds":1}`))
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "before timeout") {
		t.Fatalf("timeout=%v", err)
	}
	_, err = registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"python3","args":["-c","pass"]}`))
	if err != nil {
		t.Fatal(err)
	}
}

func TestEditPreservesCRLFAndBOM(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "windows.txt")
	if err := os.WriteFile(path, []byte("\ufefffirst\r\nold\r\nlast\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"windows.txt"}]}`)); err != nil {
		t.Fatal(err)
	}
	result, err := registry.Execute(context.Background(), callCtx, "edit_file", json.RawMessage(`{"path":"windows.txt","old_string":"old\n","new_string":"new\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\ufefffirst\r\nnew\r\nlast\r\n" || !strings.Contains(ToolResultText(result), "line 2") {
		t.Fatalf("edit changed unrelated format: %q", got)
	}
}

func TestCommandCancellationStopsDescendants(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	registry, callCtx := workspaceToolTestRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	input, _ := json.Marshal(map[string]any{"program": "python3", "args": []string{"-c", `import subprocess,time,sys
subprocess.Popen([sys.executable,"-c","import time; time.sleep(0.9); open('orphan-marker','w').write('leaked')"])
print('spawned',flush=True)
time.sleep(10)`}})
	_, err := registry.Execute(ctx, callCtx, "run_command", input)
	if err == nil || !strings.Contains(err.Error(), "spawned") {
		t.Fatalf("cancellation lost output: %v", err)
	}
	time.Sleep(time.Second)
	if _, err = os.Stat(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "orphan-marker")); !os.IsNotExist(err) {
		t.Fatalf("command descendant survived: %v", err)
	}
}
