package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
		Program        string   `json:"program"`
		Args           []string `json:"args"`
		Command        string   `json:"command"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
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
	if workspaceAccessMode(callCtx) == runtimeworkspace.AccessReadOnly {
		if err := validateReadOnlyCommand(base, args); err != nil {
			return nil, err
		}
	}
	root, err := requireWorkspaceRoot(callCtx, "run_command")
	if err != nil {
		return nil, err
	}
	if params.TimeoutSeconds == 0 {
		params.TimeoutSeconds = 120
	}
	if params.TimeoutSeconds < 1 || params.TimeoutSeconds > 900 {
		return nil, fmt.Errorf("timeout_seconds must be between 1 and 900")
	}
	timeout, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(timeout, program, args...)
	cmd.Dir = root
	// run_command lets an agent pick the program, so the child must never
	// inherit the runtime's environment: `cat /proc/self/environ` or
	// `node -e 'console.log(process.env)'` would otherwise hand back every
	// worker credential.
	if workspaceAccessMode(callCtx) == runtimeworkspace.AccessReadOnly && base == "git" {
		cmd.Env = append(procenv.Command(), "GIT_OPTIONAL_LOCKS=0")
	} else {
		cmd.Env = procenv.Command()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	output := &commandOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	err = cmd.Run()
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
