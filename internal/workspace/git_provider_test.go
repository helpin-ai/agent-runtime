package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestRepositoryProviderPreparesLocalGitWorkspace(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "README.md")
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")

	provider := RepositoryProvider{
		RootDir: tmp,
		SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{
			Provider:   "git",
			CloneURL:   remote,
			BaseBranch: "main",
			WorkBranch: "agent/run-1",
			CommitIdentity: &GitIdentity{
				Name:  "Agent",
				Email: "agent@example.com",
			},
		}},
	}
	lease, err := provider.PrepareWorkspace(context.Background(), PrepareRequest{
		AppID:       "app-a",
		RunID:       "run-1",
		AgentID:     "agent-1",
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if lease == nil || lease.RootPath == "" {
		t.Fatalf("expected lease root path, got %#v", lease)
	}
	if _, err := os.Stat(filepath.Join(lease.RootPath, "README.md")); err != nil {
		t.Fatalf("expected cloned README: %v", err)
	}
	branch := runGitOutput(t, lease.RootPath, "branch", "--show-current")
	if branch != "agent/run-1" {
		t.Fatalf("expected work branch, got %q", branch)
	}
	if spec := RepositorySpecFromLease(*lease); spec == nil || spec.CloneURL != remote {
		t.Fatalf("expected redacted repository spec in lease, got %#v", lease.Metadata)
	}
}

type staticRepositorySpecProvider struct {
	spec *RepositoryWorkspaceSpec
}

func (p staticRepositorySpecProvider) ResolveRepositoryWorkspace(context.Context, PrepareRequest) (*RepositoryWorkspaceSpec, error) {
	return p.spec, nil
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, string(output))
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return string(bytesTrimSpace(output))
}

func bytesTrimSpace(value []byte) []byte {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\n' || value[0] == '\t' || value[0] == '\r') {
		value = value[1:]
	}
	for len(value) > 0 {
		last := value[len(value)-1]
		if last != ' ' && last != '\n' && last != '\t' && last != '\r' {
			break
		}
		value = value[:len(value)-1]
	}
	return value
}
