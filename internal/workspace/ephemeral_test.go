package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolStateRootUsesOneEphemeralDirectoryPerRun(t *testing.T) {
	base := filepath.Join(t.TempDir(), "agent-runtime-ephemeral")
	t.Setenv(EphemeralRootEnv, base)
	run := filepath.Join(t.TempDir(), "app", "run")
	repoA := filepath.Join(run, "repositories", "a", "repo")
	repoB := filepath.Join(run, "repositories", "b", "repo")
	first, ephemeral, err := ToolStateRoot(repoA)
	if err != nil || !ephemeral {
		t.Fatalf("first state root: %q %v %v", first, ephemeral, err)
	}
	second, _, err := ToolStateRoot(repoB)
	if err != nil || second != first {
		t.Fatalf("repositories in one run did not share state: %q %q %v", first, second, err)
	}
	other, _, err := ToolStateRoot(filepath.Join(t.TempDir(), "analysis"))
	if err != nil || other == first {
		t.Fatalf("different runs shared state: %q %q %v", first, other, err)
	}
	if filepath.Dir(first) != base || !strings.HasPrefix(filepath.Base(first), "run-") {
		t.Fatalf("state escaped configured root: %q", first)
	}
}

func TestToolStateRootFallsBackToWorkspace(t *testing.T) {
	t.Setenv(EphemeralRootEnv, "")
	root := t.TempDir()
	state, ephemeral, err := ToolStateRoot(root)
	if err != nil || ephemeral || state != filepath.Join(root, ".agent-runtime") {
		t.Fatalf("unexpected fallback: %q %v %v", state, ephemeral, err)
	}
}

func TestEphemeralStateCleanupAndResetAreNarrow(t *testing.T) {
	base := filepath.Join(t.TempDir(), "agent-runtime-ephemeral")
	t.Setenv(EphemeralRootEnv, base)
	root := t.TempDir()
	state, _, err := ToolStateRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "cache"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeReadOnlyModuleCache(t, state)
	keep := filepath.Join(base, "operator-owned")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupToolState(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("run state remains: %v", err)
	}
	stale := filepath.Join(base, "run-stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	makeReadOnlyModuleCache(t, stale)
	if err := ResetEphemeralRoot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale run state remains: %v", err)
	}
	if body, err := os.ReadFile(keep); err != nil || string(body) != "keep" {
		t.Fatalf("reset removed unrelated entry: %q %v", body, err)
	}
}

func makeReadOnlyModuleCache(t *testing.T, root string) {
	t.Helper()
	module := filepath.Join(root, "go", "pkg", "mod", "example.com", "module@v1.0.0")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "module.go"), []byte("package module\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for current := module; current != root; current = filepath.Dir(current) {
		if err := os.Chmod(current, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	// Keep test cleanup reliable if the assertion fails before production
	// cleanup gets a chance to repair the directory permissions.
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, _ error) error {
			if entry != nil && entry.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
}

func TestEphemeralRootRejectsUnsafeConfiguration(t *testing.T) {
	for _, root := range []string{"relative", string(filepath.Separator)} {
		t.Setenv(EphemeralRootEnv, root)
		if _, _, err := ToolStateRoot(t.TempDir()); err == nil {
			t.Fatalf("accepted unsafe root %q", root)
		}
	}
	linkParent := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(linkParent, "agent-runtime-ephemeral")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EphemeralRootEnv, link)
	if _, _, err := ToolStateRoot(t.TempDir()); err == nil {
		t.Fatal("accepted symlink root")
	}
}
