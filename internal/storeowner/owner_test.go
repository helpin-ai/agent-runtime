package storeowner

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/filelock"
	"github.com/helpin-ai/agent-runtime/internal/sqliteuri"
)

func TestDatabasePathAliasesShareOneIdentity(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "data")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	fileURI := sqliteuri.File(filepath.Join(real, "runtime.db"))
	want, err := DatabasePath(filepath.Join(real, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dsn := range []string{
		filepath.Join(real, "runtime.db"),
		filepath.Join(link, "runtime.db"),
		filepath.Join(real, ".", "runtime.db") + "?_busy_timeout=5000",
		fileURI.String(),
		fileURI.String() + "?_pragma=busy_timeout(5000)&cache=shared",
		"file:" + filepath.ToSlash(filepath.Join(link, "runtime.db")),
	} {
		got, err := DatabasePath(dsn)
		if err != nil || got != want {
			t.Fatalf("DatabasePath(%q) = %q, %v; want %q", dsn, got, err, want)
		}
	}
	for _, dsn := range []string{"", ":memory:", "file::memory:", "file:agent-runtime?mode=memory&cache=shared"} {
		if got, err := DatabasePath(dsn); err != nil || got != "" {
			t.Fatalf("in-memory DSN %q resolved to %q, %v", dsn, got, err)
		}
	}
}

func TestSecondOwnerWaitsAndProceedsAfterRelease(t *testing.T) {
	db := filepath.Join(t.TempDir(), "runtime.db")
	first, err := TryAcquire(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryAcquire(db); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("second owner acquired a held store: %v", err)
	}
	var waits atomic.Int32
	var seenOwner atomic.Value
	acquired := make(chan *Owner, 1)
	go func() {
		owner, err := Acquire(context.Background(), db, func(owner string) {
			waits.Add(1)
			seenOwner.Store(owner)
		})
		if err != nil {
			t.Error(err)
		}
		acquired <- owner
	}()
	time.Sleep(700 * time.Millisecond)
	select {
	case <-acquired:
		t.Fatal("acquired while the first owner held the store")
	default:
	}
	if waits.Load() < 2 {
		t.Fatalf("waiting callback ran %d times", waits.Load())
	}
	if owner, _ := seenOwner.Load().(string); !strings.Contains(owner, "pid ") {
		t.Fatalf("owner diagnostic = %q", owner)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-acquired:
		second.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not acquire after release")
	}
}

func TestWaitingIsCancellableAndNeverSteals(t *testing.T) {
	db := filepath.Join(t.TempDir(), "runtime.db")
	first, err := TryAcquire(db)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, db, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled wait = %v", err)
	}
	if _, err := os.Stat(db + LockSuffix); err != nil {
		t.Fatalf("lock file removed by a waiter: %v", err)
	}
	if _, err := TryAcquire(db); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("ownership was stolen: %v", err)
	}
}

// A crashed owner's OS lock is released by process exit even though the
// lock file stays behind.
func TestCrashedOwnerReleasesStore(t *testing.T) {
	db := filepath.Join(t.TempDir(), "runtime.db")
	command := exec.Command(os.Args[0], "-test.run=^TestOwnerProcessHelper$")
	command.Env = append(os.Environ(), "AGENT_RUNTIME_STOREOWNER_HELPER="+db)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Process.Kill(); command.Wait() })
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "owned\n" {
		t.Fatalf("helper=%q err=%v", line, err)
	}
	if _, err := TryAcquire(db); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("store not owned by helper process: %v", err)
	}
	if owner := CurrentOwner(db); !strings.Contains(owner, "pid ") {
		t.Fatalf("owner diagnostic = %q", owner)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	command.Wait()
	owner, err := Acquire(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("store not released by crash: %v", err)
	}
	owner.Release()
}

func TestOwnerProcessHelper(t *testing.T) {
	db := os.Getenv("AGENT_RUNTIME_STOREOWNER_HELPER")
	if db == "" {
		return
	}
	if _, err := TryAcquire(db); err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString("owned\n")
	time.Sleep(time.Minute)
	os.Exit(0)
}
