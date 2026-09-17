package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
	runtimeworkspace "github.com/helpin-ai/agent-runtime/internal/workspace"
)

var defaultAllowedCommands = map[string]bool{
	"go":      true,
	"npm":     true,
	"npx":     true,
	"node":    true,
	"pnpm":    true,
	"yarn":    true,
	"make":    true,
	"git":     true,
	"ls":      true,
	"cat":     true,
	"grep":    true,
	"rg":      true,
	"find":    true,
	"head":    true,
	"tail":    true,
	"wc":      true,
	"diff":    true,
	"echo":    true,
	"mkdir":   true,
	"cp":      true,
	"mv":      true,
	"rm":      true,
	"pwd":     true,
	"python":  true,
	"python3": true,
	"pip":     true,
	"pip3":    true,
	"pytest":  true,
	"uv":      true,
	"poetry":  true,
	"cargo":   true,
	"rustc":   true,
}

func (p *workspaceToolPack) runCommand(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		WorkingDirectory string   `json:"working_directory"`
		Program          string   `json:"program"`
		Args             []string `json:"args"`
		Command          string   `json:"command"`
		TimeoutSeconds   int      `json:"timeout_seconds"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	program, args, err := normalizeCommand(params.Program, params.Args, params.Command)
	if err != nil {
		return nil, err
	}
	base := program
	if strings.Contains(base, "/") {
		base = base[strings.LastIndex(base, "/")+1:]
	}
	if !defaultAllowedCommands[base] {
		return nil, fmt.Errorf("command %q is not allowed; allowed: %v", base, allowedCommandList(defaultAllowedCommands))
	}
	if callCtx.Run != nil && callCtx.Run.Input.Metadata["delivery_mode"] == "preview" && base == "git" {
		if len(args) == 0 {
			return nil, fmt.Errorf("git subcommand is required in preview mode")
		}
		switch args[0] {
		case "status", "log", "show", "rev-parse", "ls-files", "grep", "diff", "add", "restore", "reset", "checkout", "switch", "branch", "fetch", "merge", "rebase", "cherry-pick", "ls-tree", "check-ignore", "rev-list":
		default:
			return nil, fmt.Errorf("git %s is disabled in preview mode; keep changes local without committing or publishing", args[0])
		}
	}
	if workspaceAccessMode(callCtx) == runtimeworkspace.AccessReadOnly {
		if err := validateReadOnlyCommand(base, args); err != nil {
			return nil, err
		}
	}
	root, err := requireWorkspaceRoot(callCtx, "run_command")
	if err != nil {
		return nil, err
	}
	workingDirectory, err := safeWorkspacePath(root, params.WorkingDirectory)
	if err != nil {
		return nil, fmt.Errorf("working_directory: %w", err)
	}
	info, err := os.Stat(workingDirectory)
	if err != nil {
		return nil, fmt.Errorf("working_directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("working_directory must be an existing directory")
	}
	if params.TimeoutSeconds == 0 {
		params.TimeoutSeconds = 120
	}
	if params.TimeoutSeconds < 1 || params.TimeoutSeconds > 900 {
		return nil, fmt.Errorf("timeout_seconds must be between 1 and 900")
	}
	timeout, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutSeconds)*time.Second)
	defer cancel()
	env := procenv.Command()
	_, pythonCall := ctx.Value(pythonStreamsKey{}).(*pythonStreams)
	pythonEnabled := pythonCall || (callCtx.Run != nil && AllowedSet(callCtx.Agent, callCtx.Run.Input.AllowedTools)["run_python"])
	if _, local := ctx.Value(localCommandKey{}).(LocalCommandOptions); !local && pythonEnabled {
		switch base {
		case "python", "python3", "pip", "pip3", "pytest":
			program, env, err = pythonCommandEnvironment(timeout, root, program, env)
			if err != nil {
				return nil, err
			}
		}
	}
	if callCtx.Run != nil && callCtx.Run.WorkspaceLease != nil && callCtx.Run.WorkspaceLease.Provider == "analysis" {
		if err := runtimeworkspace.CheckScratchSize(root); err != nil {
			return nil, err
		}
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-timeout.Done():
					return
				case <-ticker.C:
					if runtimeworkspace.CheckScratchSize(root) != nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	cmd := exec.CommandContext(timeout, program, args...)
	cmd.Dir = workingDirectory
	// run_command lets an agent pick the program, so the child must never
	// inherit the runtime's environment: `cat /proc/self/environ` or
	// `node -e 'console.log(process.env)'` would otherwise hand back every
	// worker credential.
	if workspaceAccessMode(callCtx) == runtimeworkspace.AccessReadOnly && base == "git" {
		cmd.Env = append(env, "GIT_OPTIONAL_LOCKS=0")
	} else {
		cmd.Env = env
	}
	if options, ok := ctx.Value(localCommandKey{}).(LocalCommandOptions); ok && options.Env != nil {
		cmd.Env = append([]string(nil), options.Env...)
		if workspaceAccessMode(callCtx) == runtimeworkspace.AccessReadOnly && base == "git" {
			cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0")
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	output := &commandOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	if options, ok := ctx.Value(localCommandKey{}).(LocalCommandOptions); ok && options.Output != nil {
		writer := io.MultiWriter(output, options.Output)
		cmd.Stdout, cmd.Stderr = writer, writer
	}
	if streams, ok := ctx.Value(pythonStreamsKey{}).(*pythonStreams); ok {
		cmd.Stdout = io.MultiWriter(output, &streams.stdout)
		cmd.Stderr = io.MultiWriter(output, &streams.stderr)
	}
	err = cmd.Run()
	if callCtx.Run != nil && callCtx.Run.WorkspaceLease != nil && callCtx.Run.WorkspaceLease.Provider == "analysis" {
		if sizeErr := runtimeworkspace.CheckScratchSize(root); sizeErr != nil {
			return nil, sizeErr
		}
	}
	result := output.String()
	if timeout.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s\nCommand timed out after %d seconds", result, params.TimeoutSeconds)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s\nCommand cancelled: %w", result, ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("%s\nCommand failed: %w", result, err)
	}
	return workspaceToolText(result), nil
}

func validateReadOnlyCommand(program string, args []string) error {
	switch program {
	case "pwd", "ls", "cat", "grep", "rg", "head", "tail", "wc", "diff", "echo":
		return nil
	case "git":
		if readOnlyGitCommand(args) {
			return nil
		}
	}
	return fmt.Errorf("command %q is not permitted in a read-only workspace", program)
}

func readOnlyGitCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	subcommand := strings.TrimSpace(args[0])
	switch subcommand {
	case "status", "log", "show", "rev-parse", "ls-files", "grep":
		return true
	case "diff":
		for _, arg := range args[1:] {
			if arg == "--output" || strings.HasPrefix(arg, "--output=") {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func workspaceAccessMode(callCtx CallContext) string {
	return runtimeworkspace.AccessMode(callCtx.Agent)
}

func normalizeCommand(program string, args []string, command string) (string, []string, error) {
	if strings.TrimSpace(program) != "" {
		return strings.TrimSpace(program), args, nil
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return "", nil, fmt.Errorf("program is required")
	}
	if strings.ContainsAny(command, "|&;<>()`$") {
		return "", nil, fmt.Errorf("shell operators are not allowed; use program + args")
	}
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return "", nil, fmt.Errorf("program is required")
	}
	return parts[0], parts[1:], nil
}

func allowedCommandList(m map[string]bool) []string {
	list := make([]string, 0, len(m))
	for k := range m {
		list = append(list, k)
	}
	return list
}
