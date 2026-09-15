package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Staged skills live inside the checkout; delivery uses `git add -A`, so the
// runtime directory must be excluded or it would be committed to the repo.
func TestExcludeRuntimeArtifactsKeepsStagedSkillsOutOfCommits(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")

	for i := 0; i < 2; i++ { // idempotent
		if err := excludeRuntimeArtifacts(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
	}
	skill := filepath.Join(repo, runtimeArtifactsDir, "skills", "repository", "x")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("real change"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "-A")
	if got := runGitOutput(t, repo, "status", "--porcelain"); got != "A  change.txt" {
		t.Fatalf("runtime artifacts must not be staged, got %q", got)
	}
	exclude, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(filepathMatches(string(exclude), "/"+runtimeArtifactsDir+"/")); n != 1 {
		t.Fatalf("expected exactly one exclude line, got %d in %q", n, exclude)
	}
}

func filepathMatches(content, pattern string) []int {
	var hits []int
	for i, line := range splitLines(content) {
		if line == pattern {
			hits = append(hits, i)
		}
	}
	return hits
}

func splitLines(content string) []string {
	var out []string
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			out = append(out, content[start:i])
			start = i + 1
		}
	}
	if start < len(content) {
		out = append(out, content[start:])
	}
	return out
}
