package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type repositoryCacheBinding struct{ base, key string }

// Bindings are worker-owned and rebuilt only from a fresh authorized host spec,
// never from agent-writable files, run input metadata, or a persisted cache path.
var repositoryCacheBindings sync.Map

// RegisterRepositoryCache binds a checkout to identities returned by the trusted
// repository spec provider. Missing identity removes the binding (private fallback).
// Callers must not pass agent-selected metadata here.
func RegisterRepositoryCache(root, appID string, metadata map[string]interface{}) {
	root = canonicalCacheWorkspace(root)
	repositoryCacheBindings.Delete(root)
	if strings.TrimSpace(os.Getenv(RepositoryCacheModeEnv)) != "repository" {
		return
	}
	workspaceID := strings.TrimSpace(stringFromMetadata(metadata, "workspace_id"))
	repositoryID := strings.TrimSpace(stringFromMetadata(metadata, "repository_id"))
	if appID == "" || workspaceID == "" || repositoryID == "" || ConfinementRoot(root) == root {
		return
	}
	namespace := strings.TrimSpace(os.Getenv(RepositoryCacheNamespaceEnv))
	if namespace == "" {
		namespace = "v1"
	}
	identity, _ := json.Marshal([]string{appID, workspaceID, repositoryID, namespace, runtime.GOOS, runtime.GOARCH})
	hash := sha256.Sum256(identity)
	base, err := repositoryCacheBase()
	if err != nil {
		return // Startup/command validation reports the configuration error.
	}
	workspaceBase := filepath.Dir(filepath.Dir(ConfinementRoot(root)))
	if pathsOverlap(base, workspaceBase) {
		return // Never grant a cache tree inside (or containing) run workspaces.
	}
	repositoryCacheBindings.Store(root, repositoryCacheBinding{base: base, key: hex.EncodeToString(hash[:])})
}

// repositoryCacheBase is provisioned by the operator on node-local storage.
// It must never fall back to the shared workspace volume or pod scratch.
func repositoryCacheBase() (string, error) {
	raw := strings.TrimSpace(os.Getenv(RepositoryCacheRootEnv))
	if !filepath.IsAbs(raw) || filepath.Clean(raw) == string(filepath.Separator) {
		return "", fmt.Errorf("%s must name a dedicated absolute local directory", RepositoryCacheRootEnv)
	}
	base := filepath.Clean(raw)
	info, err := os.Lstat(base)
	if err != nil {
		return "", fmt.Errorf("local repository cache root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("local repository cache root must be a real directory, not group/world writable")
	}
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	// These are broad read/execute grants in the command sandbox. Cache data
	// must not be readable via a system/toolchain grant belonging to every run.
	for _, system := range []string{"/usr", "/lib", "/lib64", "/bin", "/sbin", "/etc", "/opt", "/app", "/proc", "/dev"} {
		if pathsOverlap(resolved, system) {
			return "", fmt.Errorf("local repository cache root must be outside sandbox system paths")
		}
	}
	for _, other := range []string{WorkspaceRoot(), os.Getenv(EphemeralRootEnv)} {
		if other == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(other); err == nil {
			other = real
		}
		if pathsOverlap(resolved, other) {
			return "", fmt.Errorf("local repository cache root must be separate from workspace and ephemeral roots")
		}
	}
	if err := validateLocalCacheFilesystem(base); err != nil {
		return "", err
	}
	return base, nil
}

func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == string(filepath.Separator) || b == string(filepath.Separator) {
		return true
	}
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}

func canonicalCacheWorkspace(root string) string {
	// Never resolve agent-mutable symlinks when looking up authorization: a
	// replaced checkout must not inherit another checkout's registered identity.
	if absolute, err := filepath.Abs(root); err == nil {
		return absolute
	}
	return filepath.Clean(root)
}

func repositoryCacheBindingFor(root string) (repositoryCacheBinding, bool) {
	if strings.TrimSpace(os.Getenv(RepositoryCacheModeEnv)) != "repository" {
		return repositoryCacheBinding{}, false
	}
	v, ok := repositoryCacheBindings.Load(canonicalCacheWorkspace(root))
	if !ok {
		return repositoryCacheBinding{}, false
	}
	return v.(repositoryCacheBinding), true
}

func forgetRepositoryCaches(root string) {
	run := ConfinementRoot(canonicalCacheWorkspace(root))
	repositoryCacheBindings.Range(func(key, _ any) bool {
		if ConfinementRoot(key.(string)) == run {
			repositoryCacheBindings.Delete(key)
		}
		return true
	})
}

func (b repositoryCacheBinding) path() string {
	return filepath.Join(b.base, ".repository-caches", b.key)
}

// realCacheDirectory refuses symlinked roots and control/cache identity folders.
// These parents are never included in the command's writable Landlock grants.
func realCacheDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cache directory must not be a symlink: %s", path)
	}
	return nil
}

func openSharedRepositoryCache(b repositoryCacheBinding) (*os.Root, error) {
	if err := ValidateToolCacheConfig(); err != nil {
		return nil, err
	}
	if err := realCacheDirectory(filepath.Join(b.base, ".repository-caches")); err != nil {
		return nil, err
	}
	if err := realCacheDirectory(b.path()); err != nil {
		return nil, err
	}
	return os.OpenRoot(b.path())
}

// SharedRepositoryCachePath returns only the worker-authorized cache grant.
// No binding means no additional grant, even if the run fabricates cache metadata.
func SharedRepositoryCachePath(root string) string {
	if b, ok := repositoryCacheBindingFor(root); ok {
		return b.path()
	}
	return ""
}

func repositoryCacheLock(b repositoryCacheBinding) (*os.File, error) {
	control := filepath.Join(b.base, ".repository-cache-control")
	if err := realCacheDirectory(control); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(control, b.key+".lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "repository-cache-lock"), nil
}

func touchRepositoryCache(b repositoryCacheBinding) error {
	control, err := os.OpenRoot(filepath.Join(b.base, ".repository-cache-control"))
	if err != nil {
		return err
	}
	defer control.Close()
	return control.WriteFile(b.key+".used", []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600)
}

// AcquireRepositoryCache holds a shared cross-worker usage lease until the command
// exits. Package managers may execute concurrently; GC needs an exclusive lease.
func AcquireRepositoryCache(ctx context.Context, root string) (func(), error) {
	b, ok := repositoryCacheBindingFor(root)
	if !ok {
		return func() {}, nil
	}
	file, err := repositoryCacheLock(b)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_SH|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := touchRepositoryCache(b); err != nil {
		file.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if err := touchRepositoryCache(b); err != nil {
				slog.Warn("touch repository cache", "error", err)
			}
			file.Close()
		})
	}, nil
}

// Cache size is a soft per-repository-generation budget, checked only when idle.
// Active commands are never evicted. Zero disables the corresponding limit.
func repositoryCacheLimits() (time.Duration, int64, error) {
	ttl, maxBytes := 7*24*time.Hour, int64(20<<30)
	if value := os.Getenv("AGENT_RUNTIME_REPOSITORY_CACHE_TTL"); value != "" {
		var err error
		ttl, err = time.ParseDuration(value)
		if err != nil || ttl < 0 {
			return 0, 0, fmt.Errorf("invalid repository cache TTL")
		}
	}
	if value := os.Getenv("AGENT_RUNTIME_REPOSITORY_CACHE_MAX_BYTES"); value != "" {
		var err error
		maxBytes, err = strconv.ParseInt(value, 10, 64)
		if err != nil || maxBytes < 0 {
			return 0, 0, fmt.Errorf("invalid repository cache size budget")
		}
	}
	return ttl, maxBytes, nil
}

// PruneRepositoryCaches removes only idle, recognized cache directories. It
// never visits checkout trees and never follows symlinks while sizing/deleting.
func PruneRepositoryCaches(ctx context.Context, base string) error {
	ttl, maxBytes, err := repositoryCacheLimits()
	if err != nil {
		return err
	}
	directory := filepath.Join(base, ".repository-caches")
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid repository cache root")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := entry.Name()
		if len(key) != 64 || !entry.IsDir() {
			continue
		}
		if _, err := hex.DecodeString(key); err != nil {
			continue
		}
		if err := pruneRepositoryCache(ctx, repositoryCacheBinding{base, key}, ttl, maxBytes); err != nil {
			return err
		}
	}
	return nil
}

func pruneRepositoryCache(ctx context.Context, b repositoryCacheBinding, ttl time.Duration, maxBytes int64) error {
	file, err := repositoryCacheLock(b)
	if err != nil {
		return err
	}
	defer file.Close()
	// Avoid scanning actively used caches. Drop this probe fence immediately;
	// the final fence below rechecks use before eviction.
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return err
	}
	used, err := os.Stat(filepath.Join(b.base, ".repository-cache-control", b.key+".used"))
	if err != nil {
		return nil
	} // No trusted last-use marker: do not guess ownership.
	expired := ttl > 0 && time.Since(used.ModTime()) > ttl
	if !expired && maxBytes > 0 {
		var size int64
		err := filepath.WalkDir(b.path(), func(path string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			if d.Type().IsRegular() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				size += info.Size()
			}
			if size > maxBytes {
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			return err
		}
		expired = size > maxBytes
	}
	if !expired {
		return nil
	}
	// Sizing large caches may be slow: do it without an exclusive lease. Recheck use
	// under the fence before retiring anything, so commands never wait for scans.
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	latest, err := os.Stat(filepath.Join(b.base, ".repository-cache-control", b.key+".used"))
	if err != nil || !latest.ModTime().Equal(used.ModTime()) {
		return nil
	}
	// Retire the directory under the fence, then release it before deletion.
	// A new run can immediately create a cold cache rather than waiting for a
	// large remote-filesystem tree deletion. Trash is never granted to commands.
	trashPath := filepath.Join(b.base, ".repository-cache-trash")
	if err := realCacheDirectory(trashPath); err != nil {
		return err
	}
	retired := b.key + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(b.path(), filepath.Join(trashPath, retired)); err != nil {
		return err
	}
	file.Close()
	parent, err := os.OpenRoot(trashPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := parent.RemoveAll(retired); err != nil {
		return err
	}
	slog.Info("evicted idle repository cache", "cache_key", b.key)
	return nil
}

// MaintainRepositoryCaches runs bounded background maintenance, never delaying
// worker startup. Only one scanner per volume runs at a time.
func MaintainRepositoryCaches(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		if strings.TrimSpace(os.Getenv(RepositoryCacheModeEnv)) == "repository" {
			base, err := repositoryCacheBase()
			if err == nil {
				lock, err := repositoryCacheLock(repositoryCacheBinding{base, "maintenance"})
				if err == nil {
					if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil {
						scan, cancel := context.WithTimeout(ctx, 30*time.Second)
						if err := PruneRepositoryCaches(scan, base); err != nil && !errors.Is(err, context.DeadlineExceeded) {
							slog.Warn("repository cache maintenance", "error", err)
						}
						if err := pruneRetiredRepositoryCaches(scan, base); err != nil && !errors.Is(err, context.DeadlineExceeded) {
							slog.Warn("retired repository cache cleanup", "error", err)
						}
						cancel()
					}
					lock.Close()
				} else {
					slog.Warn("repository cache maintenance lock", "error", err)
				}
			} else {
				slog.Warn("repository cache maintenance configuration", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// A worker interrupted after retirement can leave only regenerable trash, never
// a half-deleted active cache. The next maintenance pass finishes that cleanup.
func pruneRetiredRepositoryCaches(ctx context.Context, base string) error {
	path := filepath.Join(base, ".repository-cache-trash")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid retired cache root")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, stamp, ok := strings.Cut(entry.Name(), "-")
		if !ok || len(key) != 64 {
			continue
		}
		if _, err := hex.DecodeString(key); err != nil {
			continue
		}
		if _, err := strconv.ParseInt(stamp, 10, 64); err != nil {
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}
