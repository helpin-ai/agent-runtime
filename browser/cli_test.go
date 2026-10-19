package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRunCLICompletesWhileDaemonKeepsOutputOpen(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "daemon.pid")
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ := strconv.Atoi(string(data))
			if pid > 0 {
				if process, err := os.FindProcess(pid); err == nil {
					_ = process.Kill()
				}
			}
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := time.Now()
	output, err := RunCLI(ctx, os.Args[0], append(os.Environ(), "AR_BROWSER_CAPTURE_TEST=parent", "AR_BROWSER_CAPTURE_PID="+pidFile), "-test.run=^TestBrowserCLIHelper$")
	if err != nil || string(output) != `{"ok":true}` || time.Since(started) >= 5*time.Second {
		t.Fatalf("CLI did not finish independently of its daemon: %q %v", output, err)
	}
}

func TestBrowserCLIHelper(t *testing.T) {
	mode := os.Getenv("AR_BROWSER_CAPTURE_TEST")
	if mode == "" {
		return
	}
	if mode == "child" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if mode == "failure" {
		os.Exit(7)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestBrowserCLIHelper$")
	child.Env = append(os.Environ(), "AR_BROWSER_CAPTURE_TEST=child")
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if child.Start() != nil {
		os.Exit(2)
	}
	if os.WriteFile(os.Getenv("AR_BROWSER_CAPTURE_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
		_ = child.Process.Kill()
		os.Exit(3)
	}
	fmt.Fprint(os.Stdout, `{"ok":true}`)
	os.Exit(0)
}

func TestRunCLIPreservesFailureAndCancellation(t *testing.T) {
	env := append(os.Environ(), "AR_BROWSER_CAPTURE_TEST=failure")
	_, err := RunCLI(t.Context(), os.Args[0], env, "-test.run=^TestBrowserCLIHelper$")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("lost CLI failure: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := RunCLI(ctx, os.Args[0], env); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
