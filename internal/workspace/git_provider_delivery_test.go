package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestRepositoryFinalizationPreservesUndeliverableWork(t *testing.T) {
	for _, outcome := range []string{agentcore.RunStatusPaused, agentcore.RunStatusFailed, agentcore.RunStatusCancelled, agentcore.RunStatusCompleted} {
		t.Run(outcome, func(t *testing.T) {
			tmp := t.TempDir()
			remote, repo := filepath.Join(tmp, "remote.git"), filepath.Join(tmp, "repo")
			runGit(t, tmp, "init", "--bare", remote)
			runGit(t, tmp, "clone", remote, repo)
			runGit(t, repo, "config", "user.name", "Test")
			runGit(t, repo, "config", "user.email", "test@example.com")
			file := filepath.Join(repo, "fixture.txt")
			if err := os.WriteFile(file, []byte("initial\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, repo, "add", "fixture.txt")
			runGit(t, repo, "commit", "-m", "initial")
			runGit(t, repo, "branch", "-M", "main")
			runGit(t, repo, "push", "-u", "origin", "main")
			runGit(t, repo, "checkout", "-b", "agent/test")
			before := runGitOutput(t, repo, "rev-parse", "HEAD")
			// Completed no-op runs have nothing to deliver. Other outcomes keep
			// their uncommitted work, including a staged file, for inspection/resume.
			want := "initial\n"
			if outcome != agentcore.RunStatusCompleted {
				want = "pending review\n"
				if err := os.WriteFile(file, []byte(want), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, repo, "add", "fixture.txt")
			}
			status := runGitOutput(t, repo, "status", "--porcelain")
			_, err := (RepositoryProvider{}).FinalizeWorkspace(context.Background(), FinalizeRequest{
				Outcome: outcome,
				Lease:   agentcore.WorkspaceLease{RootPath: repo},
				Repository: &RepositoryWorkspaceSpec{
					BaseBranch: "main", WorkBranch: "agent/test", FinalizePolicy: RepositoryFinalizePushBranch,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := runGitOutput(t, repo, "rev-parse", "HEAD"); got != before {
				t.Fatalf("finalization created a commit: %s -> %s", before, got)
			}
			if got := runGitOutput(t, repo, "status", "--porcelain"); got != status {
				t.Fatalf("finalization changed the index/worktree: %q -> %q", status, got)
			}
			if got := runGitOutput(t, tmp, "--git-dir", remote, "for-each-ref", "--format=%(refname)", "refs/heads/agent/test"); got != "" {
				t.Fatalf("finalization published a branch: %s", got)
			}
			if content, err := os.ReadFile(file); err != nil || string(content) != want {
				t.Fatalf("workspace was not preserved: content=%q error=%v", content, err)
			}
		})
	}
}
