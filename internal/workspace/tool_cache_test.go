package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cacheRepository(t *testing.T, base, app, run, repo string) string {
	t.Helper()
	root := filepath.Join(base, app, run, "repositories", repo, "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRepositoryCachesSurviveEphemeralResetAndRemainPrivate(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "workspace")
	t.Setenv(RepositoryCacheNamespaceEnv, "v1")
	t.Setenv(EphemeralRootEnv, filepath.Join(t.TempDir(), "agent-runtime-ephemeral"))
	base := t.TempDir()
	repo := cacheRepository(t, base, "app", "run-a", "repo-a")
	cache, persistent, err := OpenToolCacheRoot(repo)
	if err != nil || !persistent {
		t.Fatalf("cache: %v %v", persistent, err)
	}
	name := cache.Name()
	if err := cache.WriteFile("retained", []byte("dependency"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache.Close()
	temporary, _, err := ToolStateRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "temporary"), []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ResetEphemeralRoot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatalf("ephemeral state survived: %v", err)
	}
	if err := CleanupToolState(repo); err != nil {
		t.Fatal(err)
	}
	cache, _, err = OpenToolCacheRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if cache.Name() != name {
		t.Fatal("cache moved after reset")
	}
	if body, err := cache.ReadFile("retained"); err != nil || string(body) != "dependency" {
		t.Fatalf("cache lost: %q %v", body, err)
	}
	for _, parts := range [][3]string{{"app", "run-a", "repo-b"}, {"app", "run-b", "repo-a"}, {"other-app", "run-a", "repo-a"}} {
		other := cacheRepository(t, base, parts[0], parts[1], parts[2])
		separate, _, err := OpenToolCacheRoot(other)
		if err != nil {
			t.Fatal(err)
		}
		if separate.Name() == name {
			t.Fatal("repositories or runs share writable caches")
		}
		if _, err := separate.Stat("retained"); !os.IsNotExist(err) {
			t.Fatalf("another workspace inherited cache: %v", err)
		}
		separate.Close()
	}
}

func TestRepositoryCacheRejectsSymlinkEscape(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "workspace")
	repo := cacheRepository(t, t.TempDir(), "app", "run", "repo")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(filepath.Dir(repo), "tool-cache")); err != nil {
		t.Fatal(err)
	}
	if cache, _, err := OpenToolCacheRoot(repo); err == nil {
		cache.Close()
		t.Fatal("accepted escaping cache symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("wrote outside run: %v %v", entries, err)
	}
}

func TestRepositoryCacheModeLeavesAnalysisEphemeral(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "workspace")
	t.Setenv(EphemeralRootEnv, filepath.Join(t.TempDir(), "agent-runtime-ephemeral"))
	root := t.TempDir()
	cache, persistent, err := OpenToolCacheRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if persistent || !strings.HasPrefix(cache.Name(), os.Getenv(EphemeralRootEnv)+string(filepath.Separator)) {
		t.Fatal("analysis cache became persistent")
	}
}

func TestRepositoryCacheConfigurationAndNamespace(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "workspace")
	repo := cacheRepository(t, t.TempDir(), "app", "run", "repo")
	t.Setenv(RepositoryCacheNamespaceEnv, "toolchain-a")
	a, _, err := OpenToolCacheRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	t.Setenv(RepositoryCacheNamespaceEnv, "toolchain-b")
	b, _, err := OpenToolCacheRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.Name() == b.Name() {
		t.Fatal("cache namespace did not change")
	}
	for _, value := range []string{"../escape", "..", "/outside", strings.Repeat("x", 129)} {
		t.Setenv(RepositoryCacheNamespaceEnv, value)
		if err := ValidateToolCacheConfig(); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	t.Setenv(RepositoryCacheNamespaceEnv, "v1")
	t.Setenv(RepositoryCacheModeEnv, "typo")
	if err := ValidateToolCacheConfig(); err == nil {
		t.Fatal("accepted invalid cache mode")
	}
}

func TestRepositoryCleanupRemovesPersistentCaches(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "workspace")
	base := t.TempDir()
	repo := cacheRepository(t, base, "app", "run", "repo")
	cache, _, err := OpenToolCacheRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	path := cache.Name()
	if err := cache.WriteFile("dependency", []byte("regenerable"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache.Close()
	provider := RepositoryProvider{RootDir: base}
	if err := provider.CleanupWorkspace(context.Background(), CleanupRequest{AppID: "app", RunID: "run"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cache survived checkout cleanup: %v", err)
	}
}
