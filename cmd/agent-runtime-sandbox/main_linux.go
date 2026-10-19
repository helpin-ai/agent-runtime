// agent-runtime-sandbox runs existing workspace handlers inside the owned WSL distro.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/filelock"
	"github.com/helpin-ai/agent-runtime/internal/sandbox"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	runtimeworkspace "github.com/helpin-ai/agent-runtime/internal/workspace"
	"github.com/helpin-ai/agent-runtime/sandbox/wsl"
	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "landlock-exec" {
		os.Exit(126)
	}
	if len(os.Args) > 2 && os.Args[1] == "landlock-exec" {
		os.Exit(sandbox.RunExec(os.Args[2:]))
	}
	if len(os.Args) == 2 && os.Args[1] == "probe" {
		var response wsl.Response
		result, err := probe()
		response.Result = result
		if err != nil {
			response.Error = err.Error()
		}
		if json.NewEncoder(os.Stdout).Encode(response) != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 || os.Args[1] != "execute" {
		fmt.Fprintln(os.Stderr, "expected execute or landlock-exec")
		os.Exit(126)
	}
	var response wsl.Response
	result, err := execute()
	response.Result = result
	if err != nil {
		response.Error = err.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		os.Exit(1)
	}
}

func execute() (json.RawMessage, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("WSL command helper must run as an unprivileged user")
	}
	abi, err := sandbox.ABI()
	if err != nil || abi < 3 {
		return nil, fmt.Errorf("WSL requires Landlock ABI >= 3 (got %d): %v", abi, err)
	}
	config, err := os.ReadFile("/etc/wsl.conf")
	if err != nil || !wsl.InteropDisabled(config) {
		return nil, errors.New("WSL interop must be disabled")
	}
	// wsl --exec does not run a login shell to initialize Linux's tool PATH.
	if err := os.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin"); err != nil {
		return nil, err
	}
	raw, err := bufio.NewReader(io.LimitReader(os.Stdin, wsl.MaxMessageBytes+1)).ReadBytes('\n')
	if err != nil || len(raw) > wsl.MaxMessageBytes {
		return nil, errors.New("invalid WSL execution request")
	}
	var request wsl.Request
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, errors.New("invalid WSL execution request")
	}
	if request.Tool != "run_command" && request.Tool != "run_python" {
		return nil, errors.New("unsupported WSL tool")
	}
	if request.ReadOnly && request.Tool == "run_python" {
		return nil, errors.New("Python requires a writable workspace")
	}
	root, err := filepath.EvalSymlinks(request.Root)
	if err != nil || !filepath.IsAbs(root) || filepath.Dir(root) == root {
		return nil, errors.New("WSL requires a dedicated existing workspace")
	}
	ctx, cancel, err := lifetimeContext()
	if err != nil {
		return nil, err
	}
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	state := filepath.Join(home, ".cache", "agent-runtime", "state")
	if err := os.Setenv(runtimeworkspace.EphemeralRootEnv, state); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(home, ".cache", "agent-runtime", "locks")
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(root))
	lock, err := filelock.Open(filepath.Join(lockDir, fmt.Sprintf("%x.lock", hash)))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	for {
		err := filelock.TryLock(lock, false)
		if err == nil {
			break
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer filelock.Unlock(lock)
	tools.SetCommandSandbox(tools.CommandSandboxLandlock)
	registry := tools.NewRegistry()
	tools.RegisterWorkspaceTools(registry)
	allowed := []string{"run_command"}
	if request.PythonAllowed {
		allowed = append(allowed, "run_python")
	}
	access := "read_write"
	if request.ReadOnly {
		access = "read_only"
	}
	execution, _ := json.Marshal(map[string]any{"workspace": map[string]string{"mode": "host_prepared", "access": access}})
	agent := &agentcore.Agent{ID: request.AgentID, AppID: request.AppID, AllowedTools: allowed, ExecutionConfig: execution}
	provider := "host_prepared"
	if request.Provider == "analysis" {
		provider = "analysis"
	}
	run := &agentcore.AgentRun{ID: request.RunID, AppID: request.AppID, WorkspaceLease: &agentcore.WorkspaceLease{ID: request.LeaseID, RootPath: root, Provider: provider}}
	if request.Preview {
		run.Input.Metadata = map[string]any{"delivery_mode": "preview"}
	}
	return registry.Execute(ctx, tools.CallContext{AppID: request.AppID, RunID: request.RunID, Agent: agent, Run: run}, request.Tool, request.Input)
}

func lifetimeContext() (context.Context, context.CancelFunc, error) {
	if unix.Getpgrp() != os.Getpid() {
		if err := unix.Setpgid(0, 0); err != nil {
			return nil, nil, fmt.Errorf("own Linux helper process group: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	// EOF means the Windows runtime cancelled or died. It owns this pipe.
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
		// Bootstrap deliberately outlives a tool deadline; EOF ends its owner too.
		time.Sleep(2 * time.Second)
		_ = unix.Kill(-os.Getpid(), unix.SIGKILL)
	}()
	return ctx, cancel, nil
}
