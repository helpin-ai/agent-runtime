package store

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

func (s *SQL) SupportsRunLock() bool {
	return s != nil && s.db != nil && s.db.Dialector.Name() == "postgres"
}

// AcquireRunLock holds a PostgreSQL advisory lock on a dedicated connection.
// A lost connection cancels the returned execution context. Call release only
// AFTER execution (including child-process cancellation) has finished. Never
// return a session-locked connection to the pool.
func (s *SQL) AcquireRunLock(ctx context.Context, appID, runID string, waiting func()) (context.Context, func(), error) {
	if !s.SupportsRunLock() {
		return ctx, func() {}, fmt.Errorf("ephemeral workspaces require PostgreSQL run locks")
	}
	db, err := s.db.DB()
	if err != nil {
		return ctx, func() {}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return ctx, func() {}, err
	}
	discard := func() {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
	}
	hash := sha256.Sum256([]byte("agent-runtime:workspace\x00" + appID + "\x00" + runID))
	key := int64(binary.BigEndian.Uint64(hash[:8]))
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelAcquire()
	for {
		var locked bool
		if err := conn.QueryRowContext(acquireCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil {
			discard()
			return ctx, func() {}, fmt.Errorf("acquire run lock: %w", err)
		}
		if locked {
			break
		}
		if waiting != nil {
			waiting()
		}
		select {
		case <-acquireCtx.Done():
			discard()
			return ctx, func() {}, fmt.Errorf("wait for run lock: %w", acquireCtx.Err())
		case <-time.After(time.Second):
		}
	}
	execCtx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-execCtx.Done():
				return
			case <-ticker.C:
				probeCtx, stopProbe := context.WithTimeout(execCtx, 3*time.Second)
				var one int
				err := conn.QueryRowContext(probeCtx, "SELECT 1").Scan(&one)
				stopProbe()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	return execCtx, func() { once.Do(func() { cancel(); close(stop); <-done; discard() }) }, nil
}
