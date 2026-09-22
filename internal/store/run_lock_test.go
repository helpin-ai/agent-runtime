package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/id"
)

// Uses only advisory locks on unique keys, never application records. Opt in
// against a test/staging PostgreSQL service with direct session connections.
func TestPostgresRunLockStaging(t *testing.T) {
	dsn := os.Getenv("AGENT_RUNTIME_RUN_LOCK_TEST_DSN")
	if dsn == "" {
		t.Skip("requires explicit staging PostgreSQL DSN")
	}
	s, err := OpenSQL(SQLConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal("connect to test PostgreSQL failed")
	}
	db, _ := s.db.DB()
	defer db.Close()
	ctx := context.Background()
	run := id.New("lock-smoke")
	held, release, err := s.AcquireRunLock(ctx, "smoke", run, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	waitCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	_, unlock, err := s.AcquireRunLock(waitCtx, "smoke", run, nil)
	unlock()
	if err == nil {
		t.Fatal("second connection acquired active run")
	}
	_, other, err := s.AcquireRunLock(ctx, "smoke", run+"-other", nil)
	if err != nil {
		t.Fatal("unrelated run blocked", err)
	}
	other()
	release()
	select {
	case <-held.Done():
	default:
		t.Fatal("release failed to cancel context")
	}
	held, release, err = s.AcquireRunLock(ctx, "smoke", run, nil)
	if err != nil {
		t.Fatal("released run could not be acquired", err)
	}
	defer release()
	hash := sha256.Sum256([]byte("agent-runtime:workspace\x00smoke\x00" + run))
	var terminated bool
	err = db.QueryRowContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND classid=$1::oid AND objid=$2::oid AND objsubid=1 AND granted`, int64(binary.BigEndian.Uint32(hash[:4])), int64(binary.BigEndian.Uint32(hash[4:8]))).Scan(&terminated)
	if err != nil || !terminated {
		t.Fatal("terminate only the test lock connection", err)
	}
	select {
	case <-held.Done():
	case <-time.After(6 * time.Second):
		t.Fatal("lost PostgreSQL lock did not cancel execution")
	}
	release()
	_, unlock, err = s.AcquireRunLock(ctx, "smoke", run, nil)
	if err != nil {
		t.Fatal("replacement could not acquire run after connection loss", err)
	}
	unlock()
}
