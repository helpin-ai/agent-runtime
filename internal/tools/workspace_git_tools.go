package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	gitCommandTimeout       = 60 * time.Second
	staleGitIndexLockMinAge = gitCommandTimeout + 5*time.Second
)

func (p *workspaceToolPack) createBranch(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if strings.TrimSpace(params.Name) == "" {
		return nil, fmt.Errorf("branch name is required")
	}
	root, err := requireWorkspaceRoot(callCtx, "create_branch")
	if err != nil {
		return nil, err
	}
	previousBranch, err := runWorkspaceGit(ctx, root, "branch", "--show-current")
	if err != nil {
		return nil, fmt.Errorf("get current branch: %s", strings.TrimSpace(previousBranch))
	}
	previousBranch = strings.TrimSpace(previousBranch)
	out, err := runWorkspaceGit(ctx, root, "checkout", "-b", params.Name)
	if err != nil {
		return nil, fmt.Errorf("create branch: %s", strings.TrimSpace(out))
	}
	if updater, ok := callCtx.WorkspaceManager.(interface {
		SetRepositoryBranch(context.Context, string) error
	}); ok {
		if err := updater.SetRepositoryBranch(ctx, params.Name); err != nil {
			if previousBranch != "" {
				if rollback, rollbackErr := runWorkspaceGit(ctx, root, "checkout", previousBranch); rollbackErr != nil {
					return nil, fmt.Errorf("persist repository branch: %w; restore branch: %s", err, strings.TrimSpace(rollback))
				}
			}
			return nil, fmt.Errorf("persist repository branch: %w", err)
		}
	}
	return workspaceToolText(fmt.Sprintf("Created and switched to branch %q", params.Name)), nil
}

func (p *workspaceToolPack) commitAndPush(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}
	if strings.TrimSpace(params.Message) == "" {
		return nil, fmt.Errorf("commit message is required")
	}
	root, err := requireWorkspaceRoot(callCtx, "commit_and_push")
	if err != nil {
		return nil, err
	}
	if publisher, ok := callCtx.WorkspaceManager.(interface {
		PushRepository(context.Context, string) (json.RawMessage, error)
	}); ok {
		return publisher.PushRepository(ctx, params.Message)
	}
	if err := validateWorkspaceGitNoUnresolvedConflicts(ctx, root); err != nil {
		return nil, err
	}
	if out, err := runWorkspaceGit(ctx, root, "add", "-A"); err != nil {
		return nil, fmt.Errorf("git add: %s", strings.TrimSpace(out))
	}
	if err := validateWorkspaceGitStagedChanges(ctx, root); err != nil {
		return nil, err
	}
	if out, err := runWorkspaceGit(ctx, root, "commit", "-m", params.Message); err != nil {
		return nil, fmt.Errorf("git commit: %s", strings.TrimSpace(out))
	}
	branch, err := runWorkspaceGit(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("get branch: %s", strings.TrimSpace(branch))
	}
	branch = strings.TrimSpace(branch)
	if err := pushWorkspaceGitBranchSafely(ctx, root, branch); err != nil {
		return nil, err
	}
	sha, _ := runWorkspaceGit(ctx, root, "rev-parse", "HEAD")
	return workspaceToolText(fmt.Sprintf("Committed and pushed to %s (SHA: %s)", branch, strings.TrimSpace(sha))), nil
}

func validateWorkspaceGitNoUnresolvedConflicts(ctx context.Context, root string) error {
	unmerged, err := runWorkspaceGit(ctx, root, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return fmt.Errorf("verify merge resolution: %s", strings.TrimSpace(firstNonEmptyString(unmerged, err.Error())))
	}
	if files := strings.Fields(strings.TrimSpace(unmerged)); len(files) > 0 {
		return fmt.Errorf("cannot commit repository changes while merge conflicts remain unresolved: %s", strings.Join(files, ", "))
	}
	return nil
}

func validateWorkspaceGitStagedChanges(ctx context.Context, root string) error {
	check, err := runWorkspaceGit(ctx, root, "diff", "--cached", "--check")
	if err != nil {
		normalized := strings.ToLower(check)
		if strings.Contains(normalized, "leftover conflict marker") || strings.Contains(normalized, "conflict marker") {
			return fmt.Errorf("cannot commit repository changes while merge conflict markers remain in staged files: %s", strings.TrimSpace(check))
		}
		return fmt.Errorf("verify staged changes: %s", strings.TrimSpace(firstNonEmptyString(check, err.Error())))
	}
	return nil
}

func pushWorkspaceGitBranchSafely(ctx context.Context, root, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" || branch == "HEAD" {
		return fmt.Errorf("git push: branch name is required")
	}
	_, _ = runWorkspaceGit(ctx, root, "fetch", "origin", branch+":refs/remotes/origin/"+branch)
	if _, err := runWorkspaceGit(ctx, root, "rev-parse", "--verify", "--quiet", "origin/"+branch); err == nil {
		if err := mergeWorkspaceGitRemoteBranchBeforePush(ctx, root, branch); err != nil {
			return err
		}
	}
	if out, err := runWorkspaceGit(ctx, root, "push", "-u", "origin", branch); err != nil {
		if !isWorkspaceGitNonFastForwardPushError(firstNonEmptyString(out, err.Error())) {
			return fmt.Errorf("git push: %s", strings.TrimSpace(firstNonEmptyString(out, err.Error())))
		}
		if err := mergeWorkspaceGitRemoteBranchBeforePush(ctx, root, branch); err != nil {
			return err
		}
		if retryOut, retryErr := runWorkspaceGit(ctx, root, "push", "-u", "origin", branch); retryErr != nil {
			return fmt.Errorf("git push: push still rejected after fetching and merging remote work branch %q: %s", branch, strings.TrimSpace(firstNonEmptyString(retryOut, retryErr.Error())))
		}
	}
	return nil
}

func mergeWorkspaceGitRemoteBranchBeforePush(ctx context.Context, root, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" || branch == "HEAD" {
		return fmt.Errorf("git push: branch name is required")
	}
	if out, err := runWorkspaceGit(ctx, root, "fetch", "origin", branch+":refs/remotes/origin/"+branch); err != nil {
		return fmt.Errorf("git push: fetch remote work branch before push: %s", strings.TrimSpace(firstNonEmptyString(out, err.Error())))
	}
	if _, err := runWorkspaceGit(ctx, root, "merge", "--no-ff", "--no-edit", "origin/"+branch); err == nil {
		return nil
	}
	unmerged, conflictErr := runWorkspaceGit(ctx, root, "diff", "--name-only", "--diff-filter=U")
	_, _ = runWorkspaceGit(ctx, root, "merge", "--abort")
	if conflictErr == nil {
		if files := strings.Fields(strings.TrimSpace(unmerged)); len(files) > 0 {
			return fmt.Errorf("git push: remote work branch %q has changes that conflict with local changes: %s", branch, strings.Join(files, ", "))
		}
	}
	return fmt.Errorf("git push: merge remote work branch %q before push", branch)
}

func isWorkspaceGitNonFastForwardPushError(text string) bool {
	normalized := strings.ToLower(text)
	return strings.Contains(normalized, "non-fast-forward") ||
		strings.Contains(normalized, "fetch first") ||
		strings.Contains(normalized, "updates were rejected") ||
		strings.Contains(normalized, "tip of your current branch is behind") ||
		strings.Contains(normalized, "failed to update ref")
}

func (p *workspaceToolPack) listCommits(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Branch string `json:"branch"`
		Since  string `json:"since"`
		Until  string `json:"until"`
		Path   string `json:"path"`
		Limit  int    `json:"limit"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &params); err != nil {
			return nil, fmt.Errorf("parse input: %w", err)
		}
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "list_commits", params.repoSelector())
	if err != nil {
		return nil, err
	}
	branch := strings.TrimSpace(params.Branch)
	since := strings.TrimSpace(params.Since)
	until := strings.TrimSpace(params.Until)
	path := strings.TrimSpace(params.Path)
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	ref := "HEAD"
	if branch != "" {
		if since != "" {
			_, _ = runWorkspaceGit(ctx, root, "fetch", "--shallow-since", since, "origin", branch)
		} else {
			_, _ = runWorkspaceGit(ctx, root, "fetch", fmt.Sprintf("--deepen=%d", limit), "origin", branch)
		}
		ref = "origin/" + branch
	} else if since != "" {
		_, _ = runWorkspaceGit(ctx, root, "fetch", "--shallow-since", since)
	} else {
		_, _ = runWorkspaceGit(ctx, root, "fetch", fmt.Sprintf("--deepen=%d", limit))
	}

	const fieldSep = "\x1f"
	const recordSep = "\x1e"
	format := strings.Join([]string{"%H", "%h", "%an", "%ae", "%aI", "%s"}, fieldSep) + recordSep
	args := []string{"log", "--no-color", "--pretty=format:" + format, fmt.Sprintf("-n%d", limit)}
	if since != "" {
		args = append(args, "--since", since)
	}
	if until != "" {
		args = append(args, "--until", until)
	}
	args = append(args, ref)
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := runWorkspaceGit(ctx, root, args...)
	if err != nil {
		return nil, fmt.Errorf("git log: %s", strings.TrimSpace(out))
	}
	type commitSummary struct {
		SHA      string `json:"sha"`
		ShortSHA string `json:"short_sha"`
		Author   string `json:"author"`
		Email    string `json:"email"`
		Date     string `json:"date"`
		Subject  string `json:"subject"`
	}
	commits := make([]commitSummary, 0, limit)
	for _, raw := range strings.Split(out, recordSep) {
		line := strings.Trim(raw, "\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, fieldSep)
		if len(fields) < 6 {
			continue
		}
		commits = append(commits, commitSummary{SHA: fields[0], ShortSHA: fields[1], Author: fields[2], Email: fields[3], Date: fields[4], Subject: fields[5]})
	}
	if len(commits) == 0 {
		return workspaceToolText("No commits found for the given filters."), nil
	}
	payload, _ := json.Marshal(map[string]any{
		"ref":     ref,
		"count":   len(commits),
		"commits": commits,
	})
	return payload, nil
}

func runWorkspaceGit(ctx context.Context, root string, args ...string) (string, error) {
	out, err := runWorkspaceGitOnce(ctx, root, args...)
	if err == nil || !isGitIndexLockError(out, err) {
		return out, err
	}
	recovered, recoveryErr := recoverStaleGitIndexLock(ctx, root)
	if recoveryErr != nil || !recovered {
		return out, err
	}
	return runWorkspaceGitOnce(ctx, root, args...)
}

func runWorkspaceGitOnce(ctx context.Context, root string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", args...)
	cmd.Dir = root
	env := procenv.Command()
	if local, ok := ctx.Value(localCommandKey{}).(LocalCommandOptions); ok {
		env = local.Env
		if env == nil {
			env = os.Environ()
		}
	}
	cmd.Env = append(append([]string(nil), env...), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never")
	// Git's HTTPS helpers can outlive Git and keep CombinedOutput's pipes
	// open. Cancel the whole process group, as run_command does.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if cmdCtx.Err() != nil {
		err = cmdCtx.Err()
		out = []byte(strings.TrimSpace(string(out)) + "\nGit command stopped: " + err.Error())
	}
	return string(out), err
}

func isGitIndexLockError(output string, err error) bool {
	if err == nil {
		return false
	}
	normalized := strings.ToLower(output)
	return strings.Contains(normalized, "index.lock") &&
		(strings.Contains(normalized, "another git process seems to be running") ||
			strings.Contains(normalized, "unable to create"))
}

func recoverStaleGitIndexLock(ctx context.Context, root string) (bool, error) {
	gitDir, err := resolveGitDirPath(ctx, root)
	if err != nil {
		return false, err
	}
	lockPath := filepath.Join(gitDir, "index.lock")
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if time.Since(info.ModTime()) < staleGitIndexLockMinAge {
		return false, nil
	}
	if err := os.Remove(lockPath); err != nil {
		return false, err
	}
	return true, nil
}

func resolveGitDirPath(ctx context.Context, root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("workdir is required to resolve git directory")
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", "rev-parse", "--git-dir")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("resolve git dir: %s", message)
	}
	gitDir := strings.TrimSpace(string(out))
	if gitDir == "" {
		return "", fmt.Errorf("resolve git dir: git returned an empty path")
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	return filepath.Clean(gitDir), nil
}
