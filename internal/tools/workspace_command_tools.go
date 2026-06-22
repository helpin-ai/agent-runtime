package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

var defaultAllowedCommands = map[string]bool{
	"go":     true,
	"npm":    true,
	"npx":    true,
	"node":   true,
	"make":   true,
	"git":    true,
	"ls":     true,
	"cat":    true,
	"grep":   true,
	"find":   true,
	"head":   true,
	"tail":   true,
	"wc":     true,
	"diff":   true,
	"echo":   true,
	"mkdir":  true,
	"cp":     true,
	"mv":     true,
	"rm":     true,
	"pwd":    true,
	"python": true,
	"pip":    true,
	"cargo":  true,
	"rustc":  true,
}

func (p *workspaceToolPack) runCommand(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Program string   `json:"program"`
		Args    []string `json:"args"`
		Command string   `json:"command"`
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
	root, err := requireWorkspaceRoot(callCtx, "run_command")
	if err != nil {
		return nil, err
	}
	timeout, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(timeout, program, args...)
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	result := string(output)
	if len(result) > 50_000 {
		result = result[:50_000] + "\n... (truncated)"
	}
	if timeout.Err() == context.DeadlineExceeded {
		return workspaceToolText("Exit code: command timed out\n" + result), nil
	}
	if err != nil {
		return workspaceToolText(fmt.Sprintf("Exit code: %v\n%s", err, result)), nil
	}
	return workspaceToolText(result), nil
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
