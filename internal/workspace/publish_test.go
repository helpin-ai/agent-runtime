package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestDirectPushUsesCurrentBranchAndDoesNotAutoPublish(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, tmp, "clone", remote, seed)
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.test")
	runGit(t, seed, "commit", "--allow-empty", "-m", "initial")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "push", "-u", "origin", "main")
	p := RepositoryProvider{RootDir: tmp, SpecProvider: staticRepositorySpecProvider{spec: &RepositoryWorkspaceSpec{Provider: "git", CloneURL: remote, BaseBranch: "main", WorkBranch: "main", FinalizePolicy: RepositoryFinalizeNone}}}
	req := PrepareRequest{AppID: "app", RunID: "run", AgentID: "ask-execution", RuntimeKind: agentcore.RuntimeNativeSDK, Target: agentcore.TargetRef{Type: "repository", ID: "repo"}}
	lease, err := p.PrepareWorkspace(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.PushWorkspace(context.Background(), req, lease.RootPath, "unsafe base push"); err == nil {
		t.Fatal("base publication accepted")
	}
	runGit(t, lease.RootPath, "checkout", "-b", "requested-fix")
	if err := os.WriteFile(filepath.Join(lease.RootPath, "fix.txt"), []byte("tested fix"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := p.PushWorkspace(context.Background(), req, lease.RootPath, "Requested fix")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"pushed":true`) {
		t.Fatalf("missing push outcome: %s", result)
	}
	if got := strings.TrimSpace(runGitOutput(t, remote, "show", "requested-fix:fix.txt")); got != "tested fix" {
		t.Fatalf("remote result %q", got)
	}
	if got := strings.TrimSpace(runGitOutput(t, remote, "log", "main", "--format=%s")); got != "initial" {
		t.Fatal("base branch changed")
	}
}
