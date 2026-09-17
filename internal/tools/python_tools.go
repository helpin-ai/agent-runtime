package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
)

const maxPythonSourceBytes = 1024 * 1024

func runPythonDefinition() Definition {
	return workspaceToolDefinition("run_python", "Run Python source in a fresh interpreter with a private run-local venv. Files and installed packages persist only in this run; analysis storage (512 MiB) is deleted when the run ends and never transfers to successor runs. Install dependencies with run_command (python3 -m pip install), subject to approval. Publish important outputs before finishing the turn. output_paths explicitly selects private CSV, PNG, JSON or text artifacts (10 files, 10 MiB each, 25 MiB total).", true, map[string]any{
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
	directory := filepath.Join(root, ".agent-runtime", "python", "tmp")
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

// Python state lives beneath the writable workspace, never in the image's system environment.
func pythonCommandEnvironment(ctx context.Context, root, program string, env []string) (string, []string, error) {
	state := filepath.Join(root, ".agent-runtime", "python")
	for _, name := range []string{"tmp", "cache", "home"} {
		if err := os.MkdirAll(filepath.Join(state, name), 0700); err != nil {
			return "", nil, err
		}
	}
	venv := filepath.Join(state, "venv")
	python := filepath.Join(venv, "bin", "python3")
	env = procenv.SanitizedFrom(env, "HOME="+filepath.Join(state, "home"), "TMPDIR="+filepath.Join(state, "tmp"), "PIP_CACHE_DIR="+filepath.Join(state, "cache"), "VIRTUAL_ENV="+venv, "PIP_REQUIRE_VIRTUALENV=true", "PYTHONDONTWRITEBYTECODE=1")
	if _, err := os.Stat(python); os.IsNotExist(err) {
		timeout, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(timeout, "python3", "-I", "-m", "venv", venv)
		cmd.Env = env
		cmd.Dir = root
		output := &commandOutput{}
		cmd.Stdout = output
		cmd.Stderr = output
		if err := cmd.Run(); err != nil {
			return "", nil, fmt.Errorf("prepare private Python environment: %w: %s", err, output.String())
		}
	}
	switch program {
	case "python", "python3":
		program = python
	case "pip", "pip3", "pytest":
		program = filepath.Join(venv, "bin", program)
	}
	// Use the private environment for subprocesses started by Python too.
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + filepath.Join(venv, "bin") + ":" + strings.TrimPrefix(entry, "PATH=")
		}
	}
	return program, env, nil
}
