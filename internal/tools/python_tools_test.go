package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestPythonRetainsFilesAndPrivateEnvironment(t *testing.T) {
	registry, call := workspaceToolTestRegistry(t)
	for _, source := range []string{`import pathlib,sys,os; assert sys.prefix != sys.base_prefix; pathlib.Path('data.txt').write_text('retained'); import site; (pathlib.Path(site.getsitepackages()[0])/'run_dependency.py').write_text('value = 42'); print(os.environ['PIP_CACHE_DIR'])`, `import pathlib,run_dependency; assert run_dependency.value == 42; print(pathlib.Path('data.txt').read_text())`} {
		input, _ := json.Marshal(map[string]any{"source": source})
		output, err := registry.Execute(context.Background(), call, "run_python", input)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(output), "run") && !strings.Contains(string(output), "retained") {
			t.Fatalf("unexpected output %s", output)
		}
	}
	for _, input := range []string{`{"source":"pass","timeout_seconds":301}`, `{"source":"pass","unknown":true}`, `{"source":""}`} {
		if _, err := registry.Execute(context.Background(), call, "run_python", json.RawMessage(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	secret := "SOURCE_MUST_NOT_APPEAR_IN_ARGV_9c5e"
	source := `import pathlib; data = pathlib.Path('/proc/self/cmdline').read_bytes(); assert b'` + secret + `' not in data; print('argv-hidden') # ` + secret
	input, _ := json.Marshal(map[string]any{"source": source})
	output, err := registry.Execute(context.Background(), call, "run_python", input)
	if err != nil || !strings.Contains(string(output), "argv-hidden") {
		t.Fatalf("source leaked through argv: output=%s err=%v", output, err)
	}
	matches, err := filepath.Glob(filepath.Join(call.Run.WorkspaceLease.RootPath, ".agent-runtime", "python", "tmp", "source-*.py"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary Python sources retained: %v err=%v", matches, err)
	}
	oversized, _ := json.Marshal(map[string]any{"source": strings.Repeat("x", maxPythonSourceBytes+1)})
	if _, err := registry.Execute(context.Background(), call, "run_python", oversized); err == nil || !strings.Contains(err.Error(), "source exceeds") {
		t.Fatalf("oversized source accepted: %v", err)
	}
}

func TestAnalysisPrivateOutputSelection(t *testing.T) {
	uploads := 0
	var stored []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer host-secret" {
			t.Error("missing host auth")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("artifact_type") != "analysis_output" {
			t.Error("wrong artifact type")
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		stored, err = io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Error(err)
		}
		uploads++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"artifact_id":"private-1","artifact_ref":"host-artifact://private-1","visibility":"private"}`))
	}))
	defer server.Close()
	registry, call := workspaceToolTestRegistry(t)
	RegisterAnalysisTools(registry, BrowserToolsConfig{ArtifactUploadURL: server.URL, ArtifactUploadToken: "host-secret"})
	root := call.Run.WorkspaceLease.RootPath
	if err := os.WriteFile(filepath.Join(root, "result.csv"), []byte("a,b\n1,2"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Execute(context.Background(), call, "publish_outputs", json.RawMessage(`{"paths":["result.csv"]}`)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("private"), 0600)
	os.Symlink(outside, filepath.Join(root, "escape.txt"))
	syscall.Mkfifo(filepath.Join(root, "pipe.txt"), 0600)
	for _, path := range []string{"../secret.txt", outside, "escape.txt", "pipe.txt", "."} {
		input, _ := json.Marshal(map[string]any{"paths": []string{path}})
		if _, err := registry.Execute(context.Background(), call, "publish_outputs", input); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	if uploads != 1 {
		t.Fatalf("unexpected uploads: %d", uploads)
	}
	file, err := os.Create(filepath.Join(root, "large.txt"))
	if err != nil {
		t.Fatal(err)
	}
	file.Truncate(workspace.ScratchLimitBytes + 1)
	file.Close()
	if err := workspace.CheckScratchSize(root); err == nil {
		t.Fatal("oversized scratch accepted")
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if string(stored) != "a,b\n1,2" {
		t.Fatal("published artifact depended on scratch path")
	}
}

func TestPythonVenvRecreatedWhenHalfCreated(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	root := t.TempDir()
	venv := filepath.Join(root, ".agent-runtime", "python", "venv")
	// A first creation killed mid-ensurepip leaves bin/python3 without pip and
	// without the readiness marker.
	if err := os.MkdirAll(filepath.Join(venv, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "bin", "python3"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "half-created"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The caller's deadline is far shorter than venv creation; creation must
	// still complete because it runs detached from the tool timeout.
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	program, env, err := pythonCommandEnvironment(short, root, "python3", procenv.Command())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(venv, "half-created")); !os.IsNotExist(err) {
		t.Fatalf("half-created venv was reused: %v", err)
	}
	marker := filepath.Join(venv, pythonVenvReadyMarker)
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("readiness marker missing after creation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(venv, "bin", "pip")); err != nil {
		t.Fatalf("recreated venv has no pip: %v", err)
	}
	cmd := exec.Command(program, "-c", "import pip, sys; assert sys.prefix != sys.base_prefix")
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recreated interpreter unusable: %v: %s", err, output)
	}
	// A ready venv is reused, not rebuilt.
	if _, _, err := pythonCommandEnvironment(context.Background(), root, "python3", procenv.Command()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(marker)
	if err != nil || string(after) != string(before) {
		t.Fatalf("ready venv was rebuilt: %q -> %q err=%v", before, after, err)
	}
	// An already-cancelled caller never starts a rebuild.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, _, err := pythonCommandEnvironment(cancelled, root, "python3", procenv.Command()); err == nil {
		t.Fatal("rebuild started for a cancelled caller")
	}
}

func TestRunPythonRejectsOutputPathsBeforeExecutingWithoutUploader(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	registry, call := workspaceToolTestRegistry(t)
	root := call.Run.WorkspaceLease.RootPath
	input, _ := json.Marshal(map[string]any{"source": "import pathlib; pathlib.Path('executed.txt').write_text('ran')", "output_paths": []string{"executed.txt"}})
	_, err := registry.Execute(context.Background(), call, "run_python", input)
	if err == nil || !strings.Contains(err.Error(), "private artifact storage is not configured") {
		t.Fatalf("expected configuration error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "executed.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("source executed before the uploader check: %v", statErr)
	}
}

func TestAnalysisToolsReleaseRunStateOnClose(t *testing.T) {
	registry, call := workspaceToolTestRegistry(t)
	pack := registerAnalysisTools(registry.ForApp(call.AppID), BrowserToolsConfig{ArtifactUploadURL: "http://127.0.0.1:1", ArtifactUploadToken: "token"})
	if pack == nil {
		t.Fatal("no app-scoped pack")
	}
	pack.fileState(call)
	pack.mu.Lock()
	_, retained := pack.states[workspaceToolStateKey(call.AppID, call.RunID)]
	pack.mu.Unlock()
	if !retained {
		t.Fatal("expected per-run state before close")
	}
	if err := registry.CloseRun(context.Background(), call.AppID, call.RunID); err != nil {
		t.Fatal(err)
	}
	pack.mu.Lock()
	_, retained = pack.states[workspaceToolStateKey(call.AppID, call.RunID)]
	pack.mu.Unlock()
	if retained {
		t.Fatal("app-scoped analysis pack retained run state after CloseRun")
	}
}
