//go:build linux || darwin

package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func lockWorkspace(home, root string) (func(), error) {
	dir := filepath.Join(home, "locks")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	sum := sha256.Sum256([]byte(root))
	f, e := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%x.lock", sum)), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("cannot lock workspace %s (another CLI run may own it): %w", root, e)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

// Serialize refresh-token rotation across CLI processes sharing a profile.
func lockCredentials(ctx context.Context, home, name string) (func(), error) {
	for {
		unlock, e := lockWorkspace(home, "credential:"+name)
		if e == nil {
			return unlock, nil
		}
		if !errors.Is(e, syscall.EWOULDBLOCK) && !errors.Is(e, syscall.EAGAIN) {
			return nil, e
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
