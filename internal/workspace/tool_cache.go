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
const RepositoryCacheRootEnv = "AGENT_RUNTIME_REPOSITORY_CACHE_ROOT"

// ToolCacheRoot keeps the absolute tool-facing path alongside the confined
// filesystem handle. Go 1.25's Root.OpenRoot reports only the relative child
// name, so Root.Name must not be used to construct command environment paths.
type ToolCacheRoot struct {
	*os.Root
	path string
}

// Name returns the absolute cache path, independent of the Go toolchain.
func (r *ToolCacheRoot) Name() string { return r.path }

// ValidateToolCacheConfig rejects typos instead of silently disabling cache
// reuse. Cache lifetime depends on the selected storage, not just the mode name.
func ValidateToolCacheConfig() error {
	switch strings.TrimSpace(os.Getenv(RepositoryCacheModeEnv)) {
	case "", "ephemeral", "workspace", "repository":
	default:
		return fmt.Errorf("%s must be ephemeral, workspace, or repository", RepositoryCacheModeEnv)
	}
	if strings.TrimSpace(os.Getenv(RepositoryCacheModeEnv)) == "repository" {
		if _, err := repositoryCacheBase(); err != nil {
			return err
		}
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
func OpenToolCacheRoot(workspaceRoot string) (cache *ToolCacheRoot, persistent bool, err error) {
	if binding, ok := repositoryCacheBindingFor(workspaceRoot); ok {
		root, err := openSharedRepositoryCache(binding)
		if err != nil {
			return nil, true, err
		}
		return &ToolCacheRoot{Root: root, path: binding.path()}, true, nil
	}
	return OpenPrivateToolCacheRoot(workspaceRoot)
}

// OpenPrivateToolCacheRoot retains mutable environments within one run even when
// download/build caches are shared across runs. It never grants cross-run access.
func OpenPrivateToolCacheRoot(workspaceRoot string) (cache *ToolCacheRoot, persistent bool, err error) {
	if err := ValidateToolCacheConfig(); err != nil {
		return nil, false, err
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, false, err
	}
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
		if err != nil {
			return nil, false, err
		}
		return &ToolCacheRoot{Root: cache, path: state}, false, nil
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
	handle, err := confined.OpenRoot(rel)
	if err != nil {
		return nil, true, err
	}
	return &ToolCacheRoot{Root: handle, path: cachePath}, true, nil
}
