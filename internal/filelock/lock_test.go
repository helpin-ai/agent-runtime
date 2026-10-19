package filelock

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProcessExitReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "process.lock")
	command := exec.Command(os.Args[0], "-test.run=^TestLockProcessHelper$")
	command.Env = append(os.Environ(), "AGENT_RUNTIME_FILELOCK_HELPER="+path)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); command.Process.Kill(); command.Wait() })
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("helper=%q err=%v", line, err)
	}
	file, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := TryLock(file, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("cross-process lock failed: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	command.Wait()
	if err := TryLock(file, true); err != nil {
		t.Fatalf("dead process retained lock: %v", err)
	}
}

func TestLockProcessHelper(t *testing.T) {
	path := os.Getenv("AGENT_RUNTIME_FILELOCK_HELPER")
	if path == "" {
		return
	}
	file, err := Open(path)
	if err != nil {
		os.Exit(1)
	}
	if err := TryLock(file, true); err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString("locked\n")
	var value [1]byte
	os.Stdin.Read(value[:])
	file.Close()
	os.Exit(0)
}

func TestSharedAndExclusiveOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.lock")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := TryLock(a, false); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(b, false); err != nil {
		t.Fatal(err)
	}
	if err := Unlock(b); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(b, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("exclusive lock passed shared owner: %v", err)
	}
	a.Close()
	if err := TryLock(b, true); err != nil {
		t.Fatal(err)
	}
	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := TryLock(c, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("shared lock passed exclusive owner: %v", err)
	}
	b.Close()
	if err := TryLock(c, true); err != nil {
		t.Fatalf("close did not release ownership: %v", err)
	}
}

func TestOpenRejectsSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner")
	if err := os.WriteFile(path, []byte("do not alter"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, path+".link"); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if f, err := Open(path + ".link"); err == nil {
		f.Close()
		t.Fatal("followed a lock symlink")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "do not alter" {
		t.Fatal("symlink target changed")
	}
}
