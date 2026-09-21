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
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestPythonRetainsFilesAndPrivateEnvironment(t *testing.T) {
	t.Setenv(workspace.EphemeralRootEnv, filepath.Join(t.TempDir(), "agent-runtime-ephemeral"))
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
	state, _, err := workspace.ToolStateRoot(call.Run.WorkspaceLease.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(state, "python", "tmp", "source-*.py"))
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

func TestPythonVenvIsFastAndTargetsPipWithoutSeeding(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	root := t.TempDir()
	ephemeral := filepath.Join(t.TempDir(), "agent-runtime-ephemeral")
	t.Setenv(workspace.EphemeralRootEnv, ephemeral)
	state, enabled, err := workspace.ToolStateRoot(root)
	if err != nil || !enabled {
		t.Fatalf("ephemeral state unavailable: %q %v %v", state, enabled, err)
	}
	venv := filepath.Join(state, "python", "venv")
	// A first creation interrupted before its readiness marker is rebuilt.
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
	program, args, env, err := pythonCommandEnvironment(short, root, "python3", []string{"-c", "pass"}, procenv.Command())
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
	if _, err := os.Stat(filepath.Join(venv, "bin", "pip")); !os.IsNotExist(err) {
		t.Fatalf("ordinary Python eagerly installed pip: %v", err)
	}
	cmd := exec.Command(program, args...)
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recreated interpreter unusable: %v: %s", err, output)
	}
	// Package tooling stays in the immutable image and targets the private venv.
	pipProgram, pipArgs, pipEnv, err := pythonCommandEnvironment(context.Background(), root, "pip", []string{"--version"}, procenv.Command())
	if err != nil {
		t.Fatal(err)
	}
	if pipProgram == program || len(pipArgs) < 4 || pipArgs[0] != "-m" || pipArgs[1] != "pip" || pipArgs[2] != "--python" || pipArgs[3] != program {
		t.Fatalf("pip did not target the private interpreter: program=%q args=%q", pipProgram, pipArgs)
	}
	if !slices.Contains(pipEnv, "PIP_NO_CACHE_DIR=true") || !slices.Contains(pipEnv, "PIP_NO_COMPILE=true") {
		t.Fatalf("pip write-reduction environment missing: %q", pipEnv)
	}
	if !slices.Contains(pipEnv, "VIRTUAL_ENV="+venv) {
		t.Fatalf("private environment marker missing: %q", pipEnv)
	}
	moduleProgram, moduleArgs, _, err := pythonCommandEnvironment(context.Background(), root, "python3", []string{"-m", "pip", "install", "example"}, procenv.Command())
	if err != nil {
		t.Fatal(err)
	}
	wantModuleArgs := []string{"-m", "pip", "--python", program, "install", "example"}
	if moduleProgram != pipProgram || !slices.Equal(moduleArgs, wantModuleArgs) {
		t.Fatalf("python -m pip did not target the private interpreter: program=%q args=%q", moduleProgram, moduleArgs)
	}
	// A ready venv is reused, not rebuilt.
	if _, _, _, err := pythonCommandEnvironment(context.Background(), root, "python3", []string{"-c", "pass"}, procenv.Command()); err != nil {
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
	if _, _, _, err := pythonCommandEnvironment(cancelled, root, "python3", []string{"-c", "pass"}, procenv.Command()); err == nil {
		t.Fatal("rebuild started for a cancelled caller")
	}
}

func TestPythonEnvironmentTimeoutConfiguration(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_PYTHON_ENV_TIMEOUT", "750ms")
	if got := pythonEnvironmentTimeout(); got != 750*time.Millisecond {
		t.Fatalf("duration timeout = %s", got)
	}
	t.Setenv("AGENT_RUNTIME_PYTHON_ENV_TIMEOUT", "9")
	if got := pythonEnvironmentTimeout(); got != 9*time.Second {
		t.Fatalf("seconds timeout = %s", got)
	}
	t.Setenv("AGENT_RUNTIME_PYTHON_ENV_TIMEOUT", "invalid")
	if got := pythonEnvironmentTimeout(); got != defaultPythonEnvironmentTimeout {
		t.Fatalf("invalid timeout = %s", got)
	}
}

func TestPythonEnvironmentTimeoutIsExplicit(t *testing.T) {
	bin := t.TempDir()
	fakePython := filepath.Join(bin, "python3")
	if err := os.WriteFile(fakePython, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("AGENT_RUNTIME_PYTHON_ENV_TIMEOUT", "20ms")
	root := t.TempDir()
	err := ensurePythonVenv(context.Background(), root, filepath.Join(root, "venv"), procenv.Command())
	if err == nil || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("unexpected timeout error: %v", err)
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
