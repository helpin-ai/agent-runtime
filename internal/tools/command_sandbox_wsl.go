package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	runtimeworkspace "github.com/helpin-ai/agent-runtime/internal/workspace"
	"github.com/helpin-ai/agent-runtime/sandbox/wsl"
)

var wslCommandConfig atomic.Pointer[wsl.Config]

// ConfigureWSLCommandSandboxFromEnv enables commands only after a functional probe.
func ConfigureWSLCommandSandboxFromEnv() error {
	config := wsl.Config{Distribution: os.Getenv("AGENT_RUNTIME_WSL_DISTRIBUTION"), Helper: os.Getenv("AGENT_RUNTIME_WSL_HELPER"), ProbeRoot: os.Getenv("AGENT_RUNTIME_WSL_PROBE_ROOT")}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := config.Probe(ctx); err != nil {
		return fmt.Errorf("WSL confinement probe: %w", err)
	}
	wslCommandConfig.Store(&config)
	SetCommandSandbox(CommandSandboxWSL)
	return nil
}

func runWSLTool(ctx context.Context, call CallContext, tool string, input json.RawMessage) (json.RawMessage, error) {
	config := wslCommandConfig.Load()
	if config == nil {
		return nil, errors.New("WSL sandbox has not passed its startup probe")
	}
	root, err := requireWorkspaceRoot(call, tool)
	if err != nil {
		return nil, err
	}
	nativeRoot, err := wsl.NativePath(root)
	if err != nil {
		return nil, err
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	var outputs []string
	if tool == "run_python" {
		if raw := args["output_paths"]; raw != nil {
			if err := json.Unmarshal(raw, &outputs); err != nil {
				return nil, err
			}
			if len(outputs) > 10 {
				return nil, errors.New("output_paths allows at most 10 files")
			}
			if len(outputs) > 0 {
				if _, err := analysisUploader(ctx); err != nil {
					return nil, err
				}
			}
			args["output_paths"] = json.RawMessage(`[]`)
		}
	} else if raw := args["working_directory"]; raw != nil {
		var directory string
		if err := json.Unmarshal(raw, &directory); err != nil {
			return nil, err
		}
		if filepath.IsAbs(directory) {
			directory, err = wsl.NativePath(directory)
		} else {
			directory = filepath.ToSlash(directory)
		}
		if err != nil {
			return nil, err
		}
		args["working_directory"], _ = json.Marshal(directory)
	}
	input, err = json.Marshal(args)
	if err != nil {
		return nil, err
	}
	request := wsl.Request{Tool: tool, Input: input, Root: nativeRoot, AppID: call.AppID, RunID: call.RunID, ReadOnly: workspaceAccessMode(call) == runtimeworkspace.AccessReadOnly}
	if call.Agent != nil {
		request.AgentID = call.Agent.ID
	}
	if call.Run != nil {
		request.PythonAllowed = AllowedSet(call.Agent, call.Run.Input.AllowedTools)["run_python"]
		request.Preview = call.Run.Input.Metadata["delivery_mode"] == "preview"
		if call.Run.WorkspaceLease != nil {
			request.LeaseID = call.Run.WorkspaceLease.ID
			request.Provider = call.Run.WorkspaceLease.Provider
		}
	}
	raw, err := config.Run(ctx, "execute", request)
	if err != nil || tool != "run_python" {
		return raw, err
	}
	artifacts, err := publishAnalysisOutputs(ctx, call, root, outputs)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	result["artifacts"] = artifacts
	if scope, ok := result["workspace"].(map[string]any); ok {
		scope["command_path"] = nativeRoot
	}
	return json.Marshal(result)
}
