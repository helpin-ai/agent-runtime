package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
	runtimeworkspace "github.com/helpin-ai/agent-runtime/internal/workspace"
)

const maxPythonSourceBytes = 1024 * 1024

const defaultPythonEnvironmentTimeout = 5 * time.Minute

func runPythonDefinition() Definition {
	return workspaceToolDefinition("run_python", "Run local Python source in a fresh interpreter with a private run-local venv. For credential-free public GETs, first use fetch_url with output_path, then analyze the saved file here; network access inside Python is approval-gated. Files and installed packages persist only in this run; analysis storage (512 MiB) is deleted when the run ends and never transfers to successor runs. Install dependencies with run_command (python3 -m pip install), subject to approval. Publish important outputs before finishing the turn. output_paths explicitly selects private CSV, PNG, JSON or text artifacts (10 files, 10 MiB each, 25 MiB total).", true, map[string]any{
		"type": "object", "properties": map[string]any{
			"source":          map[string]any{"type": "string", "maxLength": maxPythonSourceBytes},
			"output_paths":    map[string]any{"type": "array", "maxItems": 10, "items": map[string]any{"type": "string"}},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 300},
		}, "required": []string{"source"}, "additionalProperties": false,
	})
}

type pythonStreamsKey struct{}
type pythonStreams struct{ stdout, stderr commandOutput }

func (p *workspaceToolPack) runPython(ctx context.Context, call CallContext, input json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Source      string   `json:"source"`
		OutputPaths []string `json:"output_paths"`
		Timeout     int      `json:"timeout_seconds"`
	}
	if err := decodeStrictWorkspaceInput(input, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Source) == "" {
		return nil, fmt.Errorf("source is required")
	}
	if len(args.Source) > maxPythonSourceBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", maxPythonSourceBytes)
	}
	if args.Timeout == 0 {
		args.Timeout = 120
	}
	if args.Timeout < 1 || args.Timeout > 300 {
		return nil, fmt.Errorf("timeout_seconds must be between 1 and 300")
	}
	if len(args.OutputPaths) > 10 {
		return nil, fmt.Errorf("output_paths allows at most 10 files")
	}
	// Fail before executing anything: the source must not run when its
	// selected outputs could never be published.
	if len(args.OutputPaths) > 0 {
		if _, err := analysisUploader(ctx); err != nil {
			return nil, err
		}
	}
	root, err := requireWorkspaceRoot(call, "run_python")
	if err != nil {
		return nil, err
	}
	sourcePath, err := writePythonSource(root, args.Source)
	if err != nil {
		return nil, err
	}
	defer os.Remove(sourcePath)
	streams := &pythonStreams{}
	cmd, _ := json.Marshal(map[string]any{"program": "python3", "args": []string{"-I", sourcePath}, "timeout_seconds": args.Timeout})
	_, err = p.runCommand(context.WithValue(ctx, pythonStreamsKey{}, streams), call, cmd)
	if err != nil {
		return nil, err
	}
	artifacts, err := publishAnalysisOutputs(ctx, call, root, args.OutputPaths)
	if err != nil {
		return nil, err
	}
	expires := "repository_lifecycle"
	if call.Run.WorkspaceLease != nil && call.Run.WorkspaceLease.Provider == "analysis" {
		expires = "run_end"
	}
	return json.Marshal(map[string]any{"stdout": streams.stdout.String(), "stderr": streams.stderr.String(), "workspace": map[string]any{"id": call.Run.WorkspaceLease.ID, "available": true, "expires": expires, "transfers_to_successor": false}, "artifacts": artifacts})
}

func writePythonSource(root, source string) (string, error) {
	state, _, err := runtimeworkspace.ToolStateRoot(root)
	if err != nil {
		return "", err
	}
	directory := filepath.Join(state, "python", "tmp")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", fmt.Errorf("prepare Python source directory: %w", err)
	}
	file, err := os.CreateTemp(directory, "source-*.py")
	if err != nil {
		return "", fmt.Errorf("create Python source file: %w", err)
	}
	path := file.Name()
	if _, err := file.WriteString(source); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("write Python source file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close Python source file: %w", err)
	}
	return path, nil
}

// pythonVenvReadyMarker is written only after the lightweight, pip-free venv
// completed. Pip runs from the immutable image and targets this environment,
// avoiding a large ensurepip copy into metadata-heavy RWX storage.
const pythonVenvReadyMarker = ".agent-runtime-ready"

// pythonVenvLocks serializes venv creation per workspace root so concurrent
// commands in one run never race to build the same environment.
var pythonVenvLocks struct {
	mu    sync.Mutex
	roots map[string]*sync.Mutex
}

func pythonVenvLock(venv string) *sync.Mutex {
	pythonVenvLocks.mu.Lock()
	defer pythonVenvLocks.mu.Unlock()
	if pythonVenvLocks.roots == nil {
		pythonVenvLocks.roots = map[string]*sync.Mutex{}
	}
	lock := pythonVenvLocks.roots[venv]
	if lock == nil {
		lock = &sync.Mutex{}
		pythonVenvLocks.roots[venv] = lock
	}
	return lock
}

// Python state is run-local and can live on operator-provided ephemeral
// storage, never in the image's system environment.
func pythonCommandEnvironment(ctx context.Context, root, program string, args []string, env []string) (string, []string, []string, error) {
	toolState, _, err := runtimeworkspace.ToolStateRoot(root)
	if err != nil {
		return "", nil, nil, err
	}
	state := filepath.Join(toolState, "python")
	for _, name := range []string{"tmp", "cache", "home"} {
		if err := os.MkdirAll(filepath.Join(state, name), 0700); err != nil {
			return "", nil, nil, err
		}
	}
	cache, persistent, err := runtimeworkspace.OpenPrivateToolCacheRoot(root)
	if err != nil {
		return "", nil, nil, err
	}
	defer cache.Close()
	venv := filepath.Join(state, "venv")
	pipCache := filepath.Join(state, "cache")
	var cacheOverrides []string
	if persistent {
		identity, err := pythonCacheIdentity(ctx)
		if err != nil {
			return "", nil, nil, err
		}
		if err := cache.MkdirAll("cache/pip", 0o700); err != nil {
			return "", nil, nil, err
		}
		venv = filepath.Join(cache.Name(), "python", "venv-"+identity)
		pipCache = filepath.Join(cache.Name(), "cache", "pip")
	} else {
		// Small analysis scratch retains the previous no-download-cache policy.
		cacheOverrides = append(cacheOverrides, "PIP_NO_CACHE_DIR=true")
	}
	if runtimeworkspace.SharedRepositoryCachePath(root) != "" {
		shared, _, err := runtimeworkspace.OpenToolCacheRoot(root)
		if err != nil {
			return "", nil, nil, err
		}
		defer shared.Close()
		if err := shared.MkdirAll("cache/pip", 0o700); err != nil {
			return "", nil, nil, err
		}
		pipCache = filepath.Join(shared.Name(), "cache", "pip")
	}
	python := filepath.Join(venv, "bin", "python3")
	cacheOverrides = append(cacheOverrides, "HOME="+filepath.Join(state, "home"), "TMPDIR="+filepath.Join(state, "tmp"), "PIP_CACHE_DIR="+pipCache, "PIP_NO_COMPILE=true", "PIP_REQUIRE_VIRTUALENV=true", "PYTHONDONTWRITEBYTECODE=1", "VIRTUAL_ENV="+venv)
	env = procenv.SanitizedFrom(env, cacheOverrides...)
	if err := ensurePythonVenv(ctx, root, venv, env); err != nil {
		return "", nil, nil, err
	}
	base := filepath.Base(program)
	switch base {
	case "python", "python3":
		if len(args) >= 2 && args[0] == "-m" && args[1] == "pip" {
			systemPython, err := exec.LookPath("python3")
			if err != nil {
				return "", nil, nil, fmt.Errorf("prepare private Python package command: %w", err)
			}
			program = systemPython
			args = append([]string{"-m", "pip", "--python", python}, args[2:]...)
		} else {
			program = python
		}
	case "pip", "pip3":
		systemPython, err := exec.LookPath("python3")
		if err != nil {
			return "", nil, nil, fmt.Errorf("prepare private Python package command: %w", err)
		}
		program = systemPython
		args = append([]string{"-m", "pip", "--python", python}, args...)
	case "pytest":
		program = python
		args = append([]string{"-m", "pytest"}, args...)
	}
	// Use the private environment for subprocesses started by Python too.
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + filepath.Join(venv, "bin") + ":" + strings.TrimPrefix(entry, "PATH=")
		}
	}
	return program, args, env, nil
}

// Venvs embed interpreter paths and must not be reused with an incompatible
// Python image. Download caches already distinguish wheels by platform/ABI.
func pythonCacheIdentity(ctx context.Context) (string, error) {
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probe, "python3", "-I", "-c", "import sys,sysconfig; print(sys.version); print(sys.executable); print(sysconfig.get_config_var('SOABI'))")
	cmd.Env = procenv.Command()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("identify Python cache compatibility: %w", err)
	}
	digest := sha256.Sum256(out)
	return fmt.Sprintf("%x", digest[:12]), nil
}

// ensurePythonVenv creates the run's private venv once without pip. Creation
// runs detached from the calling tool's deadline so a short command timeout
// cannot leave a half-created environment. Avoiding ensurepip keeps first-use
// latency bounded on metadata-heavy RWX filesystems.
func ensurePythonVenv(ctx context.Context, root, venv string, env []string) error {
	lock := pythonVenvLock(venv)
	lock.Lock()
	defer lock.Unlock()
	cache, _, err := runtimeworkspace.OpenPrivateToolCacheRoot(root)
	if err != nil {
		return err
	}
	defer cache.Close()
	rel, err := filepath.Rel(cache.Name(), venv)
	if err != nil {
		return err
	}
	python := filepath.Join(venv, "bin", "python3")
	marker := filepath.Join(rel, pythonVenvReadyMarker)
	if _, err := cache.Stat(marker); err == nil {
		if _, err := os.Stat(python); err == nil {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("prepare private Python environment: %w", err)
	}
	// Missing, or interrupted before the marker was written: rebuild from scratch.
	if err := cache.RemoveAll(rel); err != nil {
		return fmt.Errorf("reset private Python environment: %w", err)
	}
	environmentTimeout := pythonEnvironmentTimeout()
	creation, cancel := context.WithTimeout(context.WithoutCancel(ctx), environmentTimeout)
	defer cancel()
	program, args := "python3", []string{"-I", "-m", "venv", "--without-pip", venv}
	if commandSandboxEnabled() {
		program, args, err = sandboxCommand(root, root, program, args)
		if err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(creation, program, args...)
	cmd.Env = env
	cmd.Dir = root
	output := &commandOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		_ = cache.RemoveAll(rel)
		if errors.Is(creation.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("prepare private Python environment: timed out after %s", environmentTimeout)
		}
		return fmt.Errorf("prepare private Python environment: %w: %s", err, output.String())
	}
	if _, err := os.Stat(python); err != nil {
		_ = cache.RemoveAll(rel)
		return fmt.Errorf("prepare private Python environment: interpreter missing after creation: %w", err)
	}
	if err := cache.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0600); err != nil {
		_ = cache.RemoveAll(rel)
		return fmt.Errorf("mark private Python environment ready: %w", err)
	}
	return nil
}

func pythonEnvironmentTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_PYTHON_ENV_TIMEOUT"))
	if raw == "" {
		return defaultPythonEnvironmentTimeout
	}
	if duration, err := time.ParseDuration(raw); err == nil && duration > 0 {
		return duration
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultPythonEnvironmentTimeout
}
