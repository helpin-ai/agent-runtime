package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	runtimeworkspace "github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestWorkspaceGitDoesNotPromptForTerminalCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nprintf '%s %s' \"$GIT_TERMINAL_PROMPT\" \"$GCM_INTERACTIVE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GCM_INTERACTIVE", "Always")
	out, err := runWorkspaceGit(context.Background(), dir, "fetch", "origin")
	if err != nil || out != "0 Never" {
		t.Fatalf("terminal credentials must be disabled: output=%q err=%v", out, err)
	}
}

func TestWorkspaceGitCancellationStopsDescendants(t *testing.T) {
	dir := t.TempDir()
	// Like git-remote-https, this child keeps the output pipe open after its
	// parent is cancelled. A surviving child also leaves evidence on disk.
	script := "#!/bin/sh\n(sleep 0.9; printf survived > orphan-marker) &\nprintf spawned\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	out, err := runWorkspaceGit(ctx, dir, "fetch", "origin")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(out, "spawned") || !strings.Contains(out, "context deadline exceeded") {
		t.Fatalf("cancellation must preserve output and explain the timeout: output=%q err=%v", out, err)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(dir, "orphan-marker")); !os.IsNotExist(err) {
		t.Fatalf("Git descendant survived cancellation: %v", err)
	}
}

func TestWorkspaceGitRecoversFromStaleIndexLock(t *testing.T) {
	_, callCtx := workspaceGitToolTestRegistry(t)
	repoDir := callCtx.Run.WorkspaceLease.RootPath
	writeWorkspaceGitFile(t, repoDir, "tracked.txt", "hello\n")
	lockPath := filepath.Join(repoDir, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("stale"), 0644); err != nil {
		t.Fatalf("write stale index lock: %v", err)
	}
	staleTime := time.Now().Add(-staleGitIndexLockMinAge - time.Second)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("age stale index lock: %v", err)
	}

	output, err := runWorkspaceGit(context.Background(), repoDir, "add", "-A")
	if err != nil {
		t.Fatalf("git add after stale lock recovery failed: %v\noutput: %s", err, output)
	}
	if strings.Contains(strings.ToLower(output), "index.lock") {
		t.Fatalf("expected stale lock recovery, got %q", output)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale index lock to be removed, got err=%v", err)
	}
}

func TestWorkspaceGitLeavesFreshIndexLockInPlace(t *testing.T) {
	_, callCtx := workspaceGitToolTestRegistry(t)
	repoDir := callCtx.Run.WorkspaceLease.RootPath
	writeWorkspaceGitFile(t, repoDir, "tracked.txt", "hello\n")
	lockPath := filepath.Join(repoDir, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("fresh"), 0644); err != nil {
		t.Fatalf("write fresh index lock: %v", err)
	}

	out, err := runWorkspaceGit(context.Background(), repoDir, "add", "-A")
	if err == nil {
		t.Fatalf("expected git add to fail while fresh index lock exists; output=%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "index.lock") {
		t.Fatalf("expected index.lock error output, got %q", out)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected fresh index lock to remain, got err=%v", err)
	}
}

func TestWorkspaceToolListCommitsReadsLog(t *testing.T) {
	registry, callCtx := workspaceGitToolTestRegistry(t)
	repoDir := callCtx.Run.WorkspaceLease.RootPath
	commitWorkspaceGitFile(t, repoDir, "a.txt", "first commit")
	commitWorkspaceGitFile(t, repoDir, "b.txt", "second commit")

	output, err := registry.Execute(context.Background(), callCtx, "list_commits", json.RawMessage(`{"limit":10}`))
	if err != nil {
		t.Fatalf("list_commits returned error: %v", err)
	}
	var result struct {
		Count   int `json:"count"`
		Commits []struct {
			Subject string `json:"subject"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode list commits: %v; raw=%s", err, string(output))
	}
	subjects := make([]string, 0, len(result.Commits))
	for _, commit := range result.Commits {
		subjects = append(subjects, commit.Subject)
	}
	joined := strings.Join(subjects, "\n")
	if result.Count != 2 || !strings.Contains(joined, "first commit") || !strings.Contains(joined, "second commit") {
		t.Fatalf("unexpected commits: %#v raw=%s", result, string(output))
	}
}

func TestWorkspaceToolCreateBranch(t *testing.T) {
	registry, callCtx := workspaceGitToolTestRegistry(t)
	repoDir := callCtx.Run.WorkspaceLease.RootPath
	commitWorkspaceGitFile(t, repoDir, "README.md", "initial")
	output, err := registry.Execute(context.Background(), callCtx, "create_branch", json.RawMessage(`{"name":"feature/test"}`))
	if err != nil {
		t.Fatalf("create_branch returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, output), `Created and switched to branch "feature/test"`) {
		t.Fatalf("unexpected create_branch output: %q", workspaceToolString(t, output))
	}
	branch := runWorkspaceGitOutput(t, repoDir, "branch", "--show-current")
	if branch != "feature/test" {
		t.Fatalf("expected feature branch, got %q", branch)
	}
}

type recordingBranchWorkspaceManager struct {
	branch       string
	detachedHead string
	err          error
}

func (m *recordingBranchWorkspaceManager) CheckoutRepository(context.Context, CheckoutRepositoryRequest) (*CheckoutRepositoryResult, error) {
	return nil, errors.New("not implemented")
}

func (m *recordingBranchWorkspaceManager) SetRepositoryBranch(_ context.Context, branch string) error {
	m.branch = branch
	return m.err
}

func (m *recordingBranchWorkspaceManager) SetRepositoryDetachedHead(_ context.Context, commit string) error {
	m.detachedHead = commit
	return m.err
}

func TestWorkspaceToolCreateBranchPersistsLeaseBranch(t *testing.T) {
	registry, callCtx := workspaceGitToolTestRegistry(t)
	repoDir := callCtx.Run.WorkspaceLease.RootPath
	commitWorkspaceGitFile(t, repoDir, "README.md", "initial")
	manager := &recordingBranchWorkspaceManager{}
	callCtx.WorkspaceManager = manager
	if _, err := registry.Execute(context.Background(), callCtx, "create_branch", json.RawMessage(`{"name":"feature/persisted"}`)); err != nil {
		t.Fatalf("create_branch returned error: %v", err)
	}
	if manager.branch != "feature/persisted" {
		t.Fatalf("persisted branch = %q", manager.branch)
	}
}

func TestWorkspaceToolCreateBranchRestoresPreviousBranchWhenPersistenceFails(t *testing.T) {
	registry, callCtx := workspaceGitToolTestRegistry(t)
	repoDir := callCtx.Run.WorkspaceLease.RootPath
	commitWorkspaceGitFile(t, repoDir, "README.md", "initial")
	previous := runWorkspaceGitOutput(t, repoDir, "branch", "--show-current")
	callCtx.WorkspaceManager = &recordingBranchWorkspaceManager{err: errors.New("store unavailable")}
	if _, err := registry.Execute(context.Background(), callCtx, "create_branch", json.RawMessage(`{"name":"feature/rejected"}`)); err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("expected persistence error, got %v", err)
	}
	if branch := runWorkspaceGitOutput(t, repoDir, "branch", "--show-current"); branch != previous {
		t.Fatalf("branch after rollback = %q, want %q", branch, previous)
	}
}

func TestWorkspaceToolCommitAndPushToLocalRemote(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	runGitCommand(t, tmp, "init", "--bare", remote)
	seed := filepath.Join(tmp, "seed")
	runGitCommand(t, tmp, "clone", remote, seed)
	configureWorkspaceGitIdentity(t, seed)
	writeWorkspaceGitFile(t, seed, "README.md", "initial\n")
	runGitCommand(t, seed, "add", "README.md")
	runGitCommand(t, seed, "commit", "-m", "initial")
	runGitCommand(t, seed, "branch", "-M", "main")
	runGitCommand(t, seed, "push", "-u", "origin", "main")

	workDir := filepath.Join(tmp, "work")
	runGitCommand(t, tmp, "clone", remote, workDir)
	configureWorkspaceGitIdentity(t, workDir)
	runGitCommand(t, workDir, "checkout", "-b", "agent/change")
	writeWorkspaceGitFile(t, workDir, "feature.txt", "feature\n")

	registry := NewRegistry()
	run := &agentcore.AgentRun{
		ID:             "run-git-push",
		AppID:          "app-a",
		WorkspaceLease: &agentcore.WorkspaceLease{ID: "lease-1", RootPath: workDir},
	}
	callCtx := CallContext{AppID: run.AppID, RunID: run.ID, Run: run}
	output, err := registry.Execute(context.Background(), callCtx, "commit_and_push", json.RawMessage(`{"message":"agent change"}`))
	if err != nil {
		t.Fatalf("commit_and_push returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, output), "Committed and pushed to agent/change") {
		t.Fatalf("unexpected commit_and_push output: %q", workspaceToolString(t, output))
	}
	remoteSHA := runGitCommandOutput(t, tmp, "--git-dir", remote, "rev-parse", "refs/heads/agent/change")
	if remoteSHA == "" {
		t.Fatalf("expected pushed remote branch sha")
	}
}

func TestWorkspaceGitDefinitionsHaveMutatingFlags(t *testing.T) {
	registry := NewRegistry()
	readOnly, ok := registry.Definition("list_commits")
	if !ok || readOnly.Mutating {
		t.Fatalf("expected list_commits to be registered read-only, got %#v", readOnly)
	}
	for _, name := range []string{"create_branch", "commit_and_push"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s definition", name)
		}
		if !def.Mutating {
			t.Fatalf("expected %s to be mutating", name)
		}
	}
}

func workspaceGitToolTestRegistry(t *testing.T) (*Registry, CallContext) {
	t.Helper()
	repoDir := t.TempDir()
	runGitCommand(t, repoDir, "init")
	configureWorkspaceGitIdentity(t, repoDir)
	registry := NewRegistry()
	run := &agentcore.AgentRun{
		ID:             "run-git",
		AppID:          "app-a",
		WorkspaceLease: &agentcore.WorkspaceLease{ID: "lease-1", RootPath: repoDir},
	}
	return registry, CallContext{AppID: run.AppID, RunID: run.ID, Run: run}
}

func commitWorkspaceGitFile(t *testing.T, repoDir, name, message string) {
	t.Helper()
	writeWorkspaceGitFile(t, repoDir, name, message+"\n")
	runGitCommand(t, repoDir, "add", "-A")
	runGitCommand(t, repoDir, "commit", "-m", message)
}

func writeWorkspaceGitFile(t *testing.T, repoDir, name, content string) {
	t.Helper()
	path := filepath.Join(repoDir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write repo file %s: %v", name, err)
	}
}

func configureWorkspaceGitIdentity(t *testing.T, repoDir string) {
	t.Helper()
	runGitCommand(t, repoDir, "config", "user.email", "agent@example.test")
	runGitCommand(t, repoDir, "config", "user.name", "Agent")
}

func runWorkspaceGitOutput(t *testing.T, repoDir string, args ...string) string {
	t.Helper()
	out, err := runWorkspaceGit(context.Background(), repoDir, args...)
	if err != nil {
		t.Fatalf("git %v failed: %v\noutput: %s", args, err, out)
	}
	return strings.TrimSpace(out)
}

func runGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = runGitCommandOutput(t, dir, args...)
}

func runGitCommandOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\noutput: %s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

type unsupportedPushWorkspaceManager struct {
	recordingBranchWorkspaceManager
}

func (unsupportedPushWorkspaceManager) PushRepository(context.Context, string) (json.RawMessage, error) {
	return nil, runtimeworkspace.ErrDirectPublicationUnsupported
}

// Host-prepared checkouts (HTTP workspace transport) have no provider-side
// push; commit_and_push must fall back to the checkout's own credentials.
func TestWorkspaceToolCommitAndPushFallsBackWhenDirectPublicationUnsupported(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	runGitCommand(t, tmp, "init", "--bare", remote)
	seed := filepath.Join(tmp, "seed")
	runGitCommand(t, tmp, "clone", remote, seed)
	configureWorkspaceGitIdentity(t, seed)
	writeWorkspaceGitFile(t, seed, "README.md", "initial\n")
	runGitCommand(t, seed, "add", "README.md")
	runGitCommand(t, seed, "commit", "-m", "initial")
	runGitCommand(t, seed, "branch", "-M", "main")
	runGitCommand(t, seed, "push", "-u", "origin", "main")
	workDir := filepath.Join(tmp, "work")
	runGitCommand(t, tmp, "clone", remote, workDir)
	configureWorkspaceGitIdentity(t, workDir)
	runGitCommand(t, workDir, "checkout", "-b", "agent/change")
	writeWorkspaceGitFile(t, workDir, "feature.txt", "feature\n")

	registry := NewRegistry()
	run := &agentcore.AgentRun{ID: "run-git-push-fallback", AppID: "app-a", WorkspaceLease: &agentcore.WorkspaceLease{ID: "lease-1", RootPath: workDir}}
	callCtx := CallContext{AppID: run.AppID, RunID: run.ID, Run: run, WorkspaceManager: &unsupportedPushWorkspaceManager{}}
	output, err := registry.Execute(context.Background(), callCtx, "commit_and_push", json.RawMessage(`{"message":"agent change"}`))
	if err != nil {
		t.Fatalf("commit_and_push returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, output), "Committed and pushed to agent/change") {
		t.Fatalf("unexpected commit_and_push output: %q", workspaceToolString(t, output))
	}
	if runGitCommandOutput(t, tmp, "--git-dir", remote, "rev-parse", "refs/heads/agent/change") == "" {
		t.Fatalf("expected pushed remote branch sha")
	}
}
