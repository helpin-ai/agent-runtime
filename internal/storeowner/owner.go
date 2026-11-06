// Package storeowner gives one runtime process ownership of a SQLite main
// store. Lightweight recovery and work-job reconciliation assume a single
// owner; this lock makes that assumption hold when a desktop host starts a
// new runtime before the previous one has exited.
package storeowner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/filelock"
)

// LockSuffix names the lock file next to the database. It is distinct from
// SQLite's own lock files and from the memory-jobs worker lock.
const LockSuffix = ".runtime-owner.lock"

const (
	initialBackoff = 250 * time.Millisecond
	maxBackoff     = 2 * time.Second
	logInterval    = 30 * time.Second
)

// Owner holds the process lock until Release.
type Owner struct {
	file *os.File
	path string
}

// DatabasePath returns the canonical file path of a SQLite DSN, or "" when the
// DSN names an in-memory database. Plain paths, file: URIs and their query
// parameters all resolve to the same identity, so aliases contend on one lock.
func DatabasePath(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" || dsn == ":memory:" {
		return "", nil
	}
	path := dsn
	if strings.HasPrefix(dsn, "file:") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse sqlite dsn: %w", err)
		}
		if parsed.Query().Get("mode") == "memory" {
			return "", nil
		}
		path = parsed.Opaque
		if path == "" {
			path = parsed.Path
		}
		if unescaped, err := url.PathUnescape(path); err == nil {
			path = unescaped
		}
		// file:///C:/data/runtime.db carries a leading slash before the volume.
		if len(path) > 2 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
	} else if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" || path == ":memory:" {
		return "", nil
	}
	path, err := filepath.Abs(filepath.FromSlash(path))
	if err != nil {
		return "", err
	}
	// The database may not exist yet; resolve links through its directory.
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("resolve sqlite directory: %w", err)
	}
	return filepath.Join(dir, filepath.Base(path)), nil
}

// TryAcquire takes ownership without waiting. It returns filelock.ErrBusy
// when another process owns the store.
func TryAcquire(databasePath string) (*Owner, error) {
	lockPath := databasePath + LockSuffix
	file, err := filelock.Open(lockPath)
	if err != nil {
		return nil, err
	}
	if err := filelock.TryLock(file, true); err != nil {
		file.Close()
		return nil, err
	}
	// Diagnostic text only: the lock file's content and existence never
	// grant ownership, the OS lock does.
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteAt([]byte(fmt.Sprintf("pid %d since %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))), 0)
	}
	return &Owner{file: file, path: lockPath}, nil
}

// Acquire waits for ownership with bounded backoff until it succeeds or ctx
// ends. waiting is called with the current owner's diagnostic text each time
// an attempt finds the store owned; it must not block.
func Acquire(ctx context.Context, databasePath string, waiting func(owner string)) (*Owner, error) {
	backoff := initialBackoff
	var lastLog time.Time
	for {
		owner, err := TryAcquire(databasePath)
		if err == nil {
			return owner, nil
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return nil, err
		}
		current := CurrentOwner(databasePath)
		if waiting != nil {
			waiting(current)
		}
		if time.Since(lastLog) >= logInterval {
			slog.Warn("waiting for the previous runtime to release the store", "database", databasePath, "owner", current)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// CurrentOwner returns the diagnostic text the owner wrote, if readable.
func CurrentOwner(databasePath string) string {
	file, err := os.Open(databasePath + LockSuffix)
	if err != nil {
		return ""
	}
	defer file.Close()
	raw, _ := io.ReadAll(io.LimitReader(file, 256))
	return strings.TrimSpace(string(raw))
}

// Release gives up ownership. Callers stop all store use first.
func (o *Owner) Release() error {
	if o == nil || o.file == nil {
		return nil
	}
	err := filelock.Unlock(o.file)
	if closeErr := o.file.Close(); err == nil {
		err = closeErr
	}
	o.file = nil
	return err
}
