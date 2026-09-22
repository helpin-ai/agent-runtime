//go:build linux

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

// Run hold/check in separate processes on the same node/local cache mount to
// verify usage leases fence eviction across workers.
func TestRepositoryCacheLeaseStaging(t *testing.T) {
	base := os.Getenv("AGENT_RUNTIME_CACHE_LEASE_ROOT")
	if base == "" {
		t.Skip("explicit shared lease test root required")
	}
	phase := os.Getenv("AGENT_RUNTIME_CACHE_SMOKE_PHASE")
	if phase != "hold" && phase != "check" {
		t.Fatal("phase must be hold or check")
	}
	t.Setenv(workspace.RepositoryCacheModeEnv, "repository")
	cacheBase := os.Getenv(workspace.RepositoryCacheRootEnv)
	if err := workspace.ValidateToolCacheConfig(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_RUNTIME_REPOSITORY_CACHE_TTL", "1ns")
	t.Setenv("AGENT_RUNTIME_REPOSITORY_CACHE_MAX_BYTES", "0")
	root := filepath.Join(base, "app", phase, "repositories", "repo", "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace.RegisterRepositoryCache(root, "app", map[string]interface{}{"workspace_id": "ws", "repository_id": "repo"})
	mark := func(name string) {
		if err := os.WriteFile(filepath.Join(base, name), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(name string) {
		deadline := time.Now().Add(45 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(base, name)); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for " + name)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if phase == "hold" {
		release, err := workspace.AcquireRepositoryCache(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		cache, _, err := workspace.OpenToolCacheRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := cache.WriteFile("retained", []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		cache.Close()
		mark("held")
		wait("checked")
		release()
		mark("released")
	} else {
		wait("held")
		if err := workspace.PruneRepositoryCaches(context.Background(), cacheBase); err != nil {
			t.Fatal(err)
		}
		path := workspace.SharedRepositoryCachePath(root)
		if _, err := os.Stat(filepath.Join(path, "retained")); err != nil {
			t.Fatal("same-node GC evicted active cache", err)
		}
		mark("checked")
		wait("released")
		if err := workspace.PruneRepositoryCaches(context.Background(), cacheBase); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("idle cache not evicted", err)
		}
	}
}
