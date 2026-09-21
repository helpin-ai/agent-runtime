//go:build linux

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
)

// Harden before loading credentials. This protects the worker's environment and
// memory from same-uid children, but does not isolate their files or processes.
func hardenExecutionProcess() error {
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}); err != nil {
		return fmt.Errorf("disable core dumps: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("disable process dumpability: %w", err)
	}
	return nil
}
