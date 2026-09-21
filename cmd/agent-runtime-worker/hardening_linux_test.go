//go:build linux

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestExecutionParentEnvironmentIsPrivate(t *testing.T) {
	if os.Getenv("TEST_HARDENED_WORKER") == "1" {
		if err := hardenExecutionProcess(); err != nil {
			t.Fatal(err)
		}
		var limit unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_CORE, &limit); err != nil || limit.Cur != 0 || limit.Max != 0 {
			t.Fatalf("core limit: %+v %v", limit, err)
		}
		cmd := exec.Command("cat", fmt.Sprintf("/proc/%d/environ", os.Getpid()))
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		// Root in a development container may retain CAP_SYS_PTRACE. Drop uid so
		// this verifies the same-uid, unprivileged deployment behavior.
		if output, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("child read parent environment (%d bytes)", len(output))
		}
		return
	}
	binary := os.Args[0]
	if os.Getuid() == 0 {
		dir, err := os.MkdirTemp("", "hardened-worker-test-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
		payload, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		binary = filepath.Join(dir, "worker.test")
		if err := os.WriteFile(binary, payload, 0755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(binary, "-test.run=^TestExecutionParentEnvironmentIsPrivate$")
	if os.Getuid() == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	}
	cmd.Env = append(os.Environ(), "TEST_HARDENED_WORKER=1", "WORKER_SECRET=must-not-leak")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hardened subprocess: %v\n%s", err, output)
	}
}
