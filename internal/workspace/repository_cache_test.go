package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func bindCacheTest(t *testing.T, base, app, run, workspaceID, repoID string) string {
	t.Helper()
	root := cacheRepository(t, base, app, run, repoID)
	RegisterRepositoryCache(root, app, map[string]interface{}{"workspace_id": workspaceID, "repository_id": repoID})
	t.Cleanup(func() { forgetRepositoryCaches(root) })
	return root
}

func TestSharedRepositoryCacheIdentityAndCleanup(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "repository")
	t.Setenv(RepositoryCacheRootEnv, t.TempDir())
	base := t.TempDir()
	a := bindCacheTest(t, base, "app", "run-a", "workspace", "repo")
	b := bindCacheTest(t, base, "app", "run-b", "workspace", "repo")
	ca, _, err := OpenToolCacheRoot(a)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	cb, _, err := OpenToolCacheRoot(b)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	if ca.Name() != cb.Name() {
		t.Fatal("different runs do not share cache")
	}
	if err := ca.WriteFile("dependency", []byte("warm"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][3]string{{"other-app", "workspace", "repo"}, {"app", "other-workspace", "repo"}, {"app", "workspace", "other-repo"}} {
		other := bindCacheTest(t, base, identity[0], "run-other-"+identity[1]+identity[2], identity[1], identity[2])
		if SharedRepositoryCachePath(other) == ca.Name() {
			t.Fatal("cache crossed trust boundary")
		}
	}
	if err := (RepositoryProvider{RootDir: base}).CleanupWorkspace(context.Background(), CleanupRequest{AppID: "app", RunID: "run-a"}); err != nil {
		t.Fatal(err)
	}
	if body, err := cb.ReadFile("dependency"); err != nil || string(body) != "warm" {
		t.Fatalf("run cleanup removed shared cache: %v", err)
	}
	RegisterRepositoryCache(b, "app", nil)
	if SharedRepositoryCachePath(b) != "" {
		t.Fatal("missing trusted identity retained a shared grant")
	}
}

func TestSharedRepositoryCacheDoesNotFollowAuthorizationSymlink(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "repository")
	t.Setenv(RepositoryCacheRootEnv, t.TempDir())
	base := t.TempDir()
	a := bindCacheTest(t, base, "app", "run-a", "workspace-a", "repo")
	b := bindCacheTest(t, base, "app", "run-b", "workspace-b", "repo")
	want := SharedRepositoryCachePath(a)
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if got := SharedRepositoryCachePath(a); got != want {
		t.Fatal("checkout symlink changed cache authorization")
	}
	cache, _, err := OpenToolCacheRoot(a)
	if err != nil {
		t.Fatal(err)
	}
	cache.Close()
	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), want); err != nil {
		t.Fatal(err)
	}
	if cache, _, err := OpenToolCacheRoot(a); err == nil {
		cache.Close()
		t.Fatal("accepted symlinked cache identity root")
	}
}

func TestSharedRepositoryCacheGCLeasesTTLAndBudget(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "repository")
	local := t.TempDir()
	t.Setenv(RepositoryCacheRootEnv, local)
	t.Setenv("AGENT_RUNTIME_REPOSITORY_CACHE_TTL", "1ns")
	t.Setenv("AGENT_RUNTIME_REPOSITORY_CACHE_MAX_BYTES", "0")
	base := t.TempDir()
	a := bindCacheTest(t, base, "app", "run-a", "workspace", "repo")
	release, err := AcquireRepositoryCache(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	cache, _, err := OpenToolCacheRoot(a)
	if err != nil {
		t.Fatal(err)
	}
	path := cache.Name()
	if err := cache.WriteFile("item", []byte("warm"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache.Close()
	second, err := AcquireRepositoryCache(context.Background(), a)
	if err != nil {
		t.Fatal("shared users serialized", err)
	}
	second()
	if err := PruneRepositoryCaches(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("GC evicted active cache", err)
	}
	release()
	if err := PruneRepositoryCaches(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expired cache was not evicted", err)
	}
	t.Setenv("AGENT_RUNTIME_REPOSITORY_CACHE_TTL", "0")
	t.Setenv("AGENT_RUNTIME_REPOSITORY_CACHE_MAX_BYTES", "1")
	release, err = AcquireRepositoryCache(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	cache, _, err = OpenToolCacheRoot(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.WriteFile("item", []byte("warm"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache.Close()
	release()
	if err := PruneRepositoryCaches(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("oversize idle cache was not evicted", err)
	}
	// An unrecognized directory must never be deleted.
	unowned := filepath.Join(local, ".repository-caches", "user-data")
	if err := os.Mkdir(unowned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PruneRepositoryCaches(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Fatal(err)
	}
	// A missing cache simply starts cold again.
	cache, _, err = OpenToolCacheRoot(a)
	if err != nil {
		t.Fatal(err)
	}
	cache.Close()
}

func TestSharedRepositoryCacheNamespaceAndInvalidLimits(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "repository")
	t.Setenv(RepositoryCacheRootEnv, t.TempDir())
	base := t.TempDir()
	a := bindCacheTest(t, base, "app", "run-a", "workspace", "repo")
	old := SharedRepositoryCachePath(a)
	t.Setenv(RepositoryCacheNamespaceEnv, "new-toolchain")
	RegisterRepositoryCache(a, "app", map[string]interface{}{"workspace_id": "workspace", "repository_id": "repo"})
	if SharedRepositoryCachePath(a) == old {
		t.Fatal("namespace did not invalidate cache")
	}
	for _, value := range []string{"bad", "-1s"} {
		t.Setenv("AGENT_RUNTIME_REPOSITORY_CACHE_TTL", value)
		if ValidateToolCacheConfig() == nil {
			t.Fatal("accepted invalid TTL")
		}
	}
	t.Setenv("AGENT_RUNTIME_REPOSITORY_CACHE_TTL", time.Hour.String())
	t.Setenv("AGENT_RUNTIME_REPOSITORY_CACHE_MAX_BYTES", "-1")
	if ValidateToolCacheConfig() == nil {
		t.Fatal("accepted invalid size")
	}
}

func TestRepositoryLeaseUsesHostIdentityNotRunMetadata(t *testing.T) {
	t.Setenv(RepositoryCacheModeEnv, "repository")
	t.Setenv(RepositoryCacheRootEnv, t.TempDir())
	base := t.TempDir()
	a := cacheRepository(t, base, "app", "run-a", "branch-a-fingerprint")
	b := cacheRepository(t, base, "app", "run-b", "branch-b-fingerprint")
	spec := &RepositoryWorkspaceSpec{CloneURL: "https://example.com/repo.git", WorkBranch: "a", Metadata: map[string]interface{}{"workspace_id": "authorized", "repository_id": "repo"}}
	repositoryLease(PrepareRequest{AppID: "app", RunID: "run-a", Metadata: map[string]interface{}{"workspace_id": "forged"}}, spec, a, branchSyncState{})
	spec.WorkBranch = "b"
	repositoryLease(PrepareRequest{AppID: "app", RunID: "run-b"}, spec, b, branchSyncState{})
	defer forgetRepositoryCaches(a)
	defer forgetRepositoryCaches(b)
	if SharedRepositoryCachePath(a) == "" || SharedRepositoryCachePath(a) != SharedRepositoryCachePath(b) {
		t.Fatal("run input or branch fragmented authorized cache")
	}
	RegisterRepositoryCache(b, "app", map[string]interface{}{"workspace_id": "forged", "repository_id": "repo"})
	if SharedRepositoryCachePath(a) == SharedRepositoryCachePath(b) {
		t.Fatal("forged input authorized shared cache")
	}
}

func TestSharedRepositoryCacheRetiredCleanupIsBounded(t *testing.T) {
	base := t.TempDir()
	trash := filepath.Join(base, ".repository-cache-trash")
	if err := os.Mkdir(trash, 0o700); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(trash, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-123")
	if err := os.Mkdir(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	unowned := filepath.Join(trash, "unrelated")
	if err := os.Mkdir(unowned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pruneRetiredRepositoryCaches(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatal("retired cache survived", err)
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Fatal("unrelated directory removed", err)
	}
}
