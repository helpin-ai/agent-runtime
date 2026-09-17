package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
)

type reviewOutput struct{ bytes.Buffer }

func (b *reviewOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 32*1024 {
		return 0, fmt.Errorf("repository review exceeds context limit")
	}
	return b.Buffer.Write(p)
}

// Publication review includes concrete branch and changes. Unknown/untracked
// content and missing base history require a human, rather than guessing scope.
func typeSafeRepositoryContext(ctx context.Context, x *ExecutionContext) (map[string]any, error) {
	if x.WorkspaceLease == nil || x.WorkspaceLease.Provider == "analysis" {
		return nil, fmt.Errorf("publication workspace missing")
	}
	lease := x.WorkspaceLease
	runGit := func(args ...string) (string, error) {
		deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(deadline, "git", append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Dir = lease.RootPath
		cmd.Env = procenv.Command()
		var output reviewOutput
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("repository evidence unavailable")
		}
		return output.String(), nil
	}
	branch, err := runGit("branch", "--show-current")
	if err != nil {
		return nil, err
	}
	status, err := runGit("status", "--porcelain")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "??") {
			return nil, fmt.Errorf("untracked publication content requires human review")
		}
	}
	base, _ := lease.Metadata["base_branch"].(string)
	if base == "" || strings.HasPrefix(base, "-") {
		return nil, fmt.Errorf("publication base missing")
	}
	committed, err := runGit("diff", "--no-ext-diff", "--no-textconv", base+"...HEAD", "--")
	if err != nil {
		return nil, err
	}
	pending, err := runGit("diff", "--no-ext-diff", "--no-textconv", "HEAD", "--")
	if err != nil {
		return nil, err
	}
	return map[string]any{"repository_id": lease.Metadata["repository_id"], "branch": strings.TrimSpace(branch), "base_branch": base, "status": status, "untrusted_committed_diff": committed, "untrusted_pending_diff": pending}, nil
}
