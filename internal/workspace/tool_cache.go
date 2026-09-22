package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const RepositoryCacheModeEnv = "AGENT_RUNTIME_REPOSITORY_CACHE_MODE"
const RepositoryCacheNamespaceEnv = "AGENT_RUNTIME_REPOSITORY_CACHE_NAMESPACE"

// ValidateToolCacheConfig rejects typos instead of silently discarding durable
// caches. Persistent modes are opt-in; analysis scratch keeps its existing lifetime.
func ValidateToolCacheConfig() error {
	switch strings.TrimSpace(os.Getenv(RepositoryCacheModeEnv)) {
	case "", "ephemeral", "workspace", "repository":
	default:
		return fmt.Errorf("%s must be ephemeral, workspace, or repository", RepositoryCacheModeEnv)
	}
	namespace := strings.TrimSpace(os.Getenv(RepositoryCacheNamespaceEnv))
	if len(namespace) > 128 || strings.ContainsAny(namespace, `/\\`) || namespace == "." || namespace == ".." {
		return fmt.Errorf("%s must be a single cache namespace of at most 128 bytes", RepositoryCacheNamespaceEnv)
	}
	_, _, err := repositoryCacheLimits()
	return err
}

// OpenToolCacheRoot selects an authorized cross-run repository cache when bound,
// otherwise private workspace/ephemeral state. Shared callers must hold an
// AcquireRepositoryCache lease through use, including command execution.
// Create descendants through Root to reject symlink escapes.
func OpenToolCacheRoot(workspaceRoot string) (cache *os.Root, persistent bool, err error) {
	if binding, ok := repositoryCacheBindingFor(workspaceRoot); ok {
		cache, err = openSharedRepositoryCache(binding)
		return cache, true, err
	}
	return OpenPrivateToolCacheRoot(workspaceRoot)
}

// OpenPrivateToolCacheRoot retains mutable environments within one run even when
// download/build caches are shared across runs. It never grants cross-run access.
func OpenPrivateToolCacheRoot(workspaceRoot string) (cache *os.Root, persistent bool, err error) {
	if err := ValidateToolCacheConfig(); err != nil {
		return nil, false, err
	}
	root := filepath.Clean(workspaceRoot)
	runRoot := ConfinementRoot(root)
	mode := strings.TrimSpace(os.Getenv(RepositoryCacheModeEnv))
	if (mode != "workspace" && mode != "repository") || runRoot == root {
		state, _, err := ToolStateRoot(root)
		if err != nil {
			return nil, false, err
		}
		if err := os.MkdirAll(state, 0o700); err != nil {
			return nil, false, err
		}
		cache, err := os.OpenRoot(state)
		return cache, false, err
	}
	namespace := strings.TrimSpace(os.Getenv(RepositoryCacheNamespaceEnv))
	if namespace == "" {
		namespace = "v1"
	}
	cachePath := filepath.Join(filepath.Dir(root), "tool-cache", namespace+"-"+runtime.GOOS+"-"+runtime.GOARCH)
	rel, err := filepath.Rel(runRoot, cachePath)
	if err != nil {
		return nil, false, err
	}
	confined, err := os.OpenRoot(runRoot)
	if err != nil {
		return nil, false, fmt.Errorf("open repository run for cache: %w", err)
	}
	defer confined.Close()
	if err := confined.MkdirAll(rel, 0o700); err != nil {
		return nil, false, fmt.Errorf("prepare repository cache: %w", err)
	}
	cache, err = confined.OpenRoot(rel)
	return cache, true, err
}
