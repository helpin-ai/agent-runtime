package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryCacheLocalReuseAndIndependentNodes(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "repository")
	localA, localB, rwx := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv(RepositoryCacheRootEnv, localA)
	a := bindCacheTest(t, rwx, "app", "run-a", "ws", "repo")
	cache, _, err := OpenToolCacheRoot(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.WriteFile("warm", []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache.Close()
	forgetRepositoryCaches(a) // Simulate worker replacement; rebind from host spec.
	b := bindCacheTest(t, rwx, "app", "run-b", "ws", "repo")
	if _, err := os.Stat(filepath.Join(SharedRepositoryCachePath(b), "warm")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rwx, ".repository-caches")); !os.IsNotExist(err) {
		t.Fatal("shared cache was created on workspace volume", err)
	}
	t.Setenv(RepositoryCacheRootEnv, localB)
	c := bindCacheTest(t, rwx, "app", "run-c", "ws", "repo")
	if filepath.Base(SharedRepositoryCachePath(b)) != filepath.Base(SharedRepositoryCachePath(c)) {
		t.Fatal("cache identity unexpectedly depends on run or node")
	}
	other, _, err := OpenToolCacheRoot(c)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ReadFile("warm"); !os.IsNotExist(err) {
		t.Fatal("different node reused another node's bytes", err)
	}
}

func TestRepositoryCacheRequiresDedicatedLocalRoot(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "repository")
	for _, path := range []string{"", "relative", "/", filepath.Join(t.TempDir(), "missing")} {
		t.Setenv(RepositoryCacheRootEnv, path)
		if ValidateToolCacheConfig() == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	local := t.TempDir()
	t.Setenv(RepositoryCacheRootEnv, local)
	if err := ValidateToolCacheConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", local)
	if ValidateToolCacheConfig() == nil {
		t.Fatal("accepted workspace volume as cache root")
	}
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", "")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(local, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(RepositoryCacheRootEnv, link)
	if ValidateToolCacheConfig() == nil {
		t.Fatal("accepted symlink root")
	}
	t.Setenv(RepositoryCacheRootEnv, local)
	t.Setenv(EphemeralRootEnv, local)
	if ValidateToolCacheConfig() == nil {
		t.Fatal("accepted pod scratch as cache root")
	}
	t.Setenv(EphemeralRootEnv, "")
	if err := os.Chmod(local, 0o777); err != nil {
		t.Fatal(err)
	}
	if ValidateToolCacheConfig() == nil {
		t.Fatal("accepted writable parent")
	}
}

func TestCacheRootOverlap(t *testing.T) {
	for _, pair := range [][2]string{{"/", "/cache"}, {"/cache", "/"}, {"/cache", "/cache/repo"}, {"/cache/repo", "/cache"}} {
		if !pathsOverlap(pair[0], pair[1]) {
			t.Fatalf("missed overlap: %v", pair)
		}
	}
	if pathsOverlap("/cache", "/cache-other") {
		t.Fatal("prefix is not a path overlap")
	}
}
