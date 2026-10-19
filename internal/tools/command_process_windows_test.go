package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsCommandCancellationKillsDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsCommandHelper$")
	command.Env = append(os.Environ(), "RUNTIME_COMMAND_HELPER=root", "RUNTIME_COMMAND_MARKER="+marker)
	configureCommandProcess(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); command.Process.Kill(); command.Wait() })
	deadline := time.Now().Add(20 * time.Second)
	var childPID int
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(marker); err == nil {
			childPID, _ = strconv.Atoi(string(raw))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("command descendant did not start")
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(childPID))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process)
	cancel()
	if err := command.Wait(); err == nil {
		t.Fatal("cancelled command succeeded")
	}
	result, err := windows.WaitForSingleObject(process, 10000)
	if err != nil || result != windows.WAIT_OBJECT_0 {
		t.Fatalf("command descendant survived cancellation: wait=%d err=%v", result, err)
	}
}

func TestWindowsCommandHelper(t *testing.T) {
	switch os.Getenv("RUNTIME_COMMAND_HELPER") {
	case "root":
		child := exec.Command(os.Args[0], "-test.run=^TestWindowsCommandHelper$")
		child.Env = append(os.Environ(), "RUNTIME_COMMAND_HELPER=leaf")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
	case "leaf":
		if err := os.WriteFile(os.Getenv("RUNTIME_COMMAND_MARKER"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(3)
		}
	default:
		return
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}
