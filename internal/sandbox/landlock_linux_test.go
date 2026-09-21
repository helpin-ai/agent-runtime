//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

const helperRootEnv = "SANDBOX_TEST_RESTRICT_ROOT"

// TestMain doubles as the confined helper: with the root env set, the test
// binary restricts itself and execs os.Args[1:], mirroring the worker shim.
func TestMain(m *testing.M) {
	root := os.Getenv(helperRootEnv)
	if root == "" {
		os.Exit(m.Run())
	}
	runtime.LockOSThread()
	if err := Restrict(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(126)
	}
	program, err := exec.LookPath(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(126)
	}
	if err := syscall.Exec(program, os.Args[1:], os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(126)
	}
}

func requireLandlock(t *testing.T) {
	t.Helper()
	abi, err := ABI()
	if err != nil {
		t.Fatal(err)
	}
	if abi < 2 {
		t.Skipf("landlock abi %d < 2", abi)
	}
}

func confined(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	if _, err := os.Stat(root); err == nil {
		cmd.Dir = root
	}
	cmd.Env = append(os.Environ(), helperRootEnv+"="+root)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestABIProbe(t *testing.T) {
	abi, err := ABI()
	if err != nil || abi < 0 {
		t.Fatalf("abi=%d err=%v", abi, err)
	}
	if handledAccess(1)&(1<<13|1<<14) != 0 || handledAccess(2)&(1<<13) == 0 || handledAccess(3)&(1<<14) == 0 {
		t.Fatal("handled access does not follow the abi")
	}
}

func TestRestrictConfinesToRunRoot(t *testing.T) {
	requireLandlock(t)
	base := t.TempDir()
	root := filepath.Join(base, "run")
	sibling := filepath.Join(base, "other")
	for _, dir := range []string{root, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(sibling, "secret.txt")
	if err := os.WriteFile(secret, []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := confined(t, root, "cat", secret); err == nil || !strings.Contains(strings.ToLower(output), "permission denied") {
		t.Fatalf("sibling read was not denied: err=%v output=%q", err, output)
	}
	if output, err := confined(t, root, "sh", "-c", "echo ok > inside.txt && cat inside.txt && rm inside.txt"); err != nil || !strings.Contains(output, "ok") {
		t.Fatalf("write inside run root failed: err=%v output=%q", err, output)
	}
	if output, err := confined(t, root, "sh", "-c", "echo nope > "+filepath.Join(sibling, "written.txt")); err == nil || !strings.Contains(strings.ToLower(output), "permission denied") {
		t.Fatalf("sibling write was not denied: err=%v output=%q", err, output)
	}
	if _, err := exec.LookPath("python3"); err == nil {
		if output, err := confined(t, root, "python3", "-c", "print(1)"); err != nil || strings.TrimSpace(output) != "1" {
			t.Fatalf("python3 under landlock failed: err=%v output=%q", err, output)
		}
	}
}

func TestRestrictRequiresExistingRoot(t *testing.T) {
	requireLandlock(t)
	if output, err := confined(t, filepath.Join(t.TempDir(), "missing"), "true"); err == nil {
		t.Fatal("restrict with a missing root should fail")
	} else if !strings.Contains(output, "run root") {
		t.Fatalf("unexpected failure: %v %q", err, output)
	}
}

// Hosts running systemd-resolved link /etc/resolv.conf into /run; the resolver
// must still be able to read it, while the rest of /run stays out of reach.
func TestRestrictAllowsResolverConfigTarget(t *testing.T) {
	requireLandlock(t)
	target, err := filepath.EvalSymlinks("/etc/resolv.conf")
	if err != nil {
		t.Skip("no /etc/resolv.conf")
	}
	root := t.TempDir()
	if out, err := confined(t, root, "cat", target); err != nil {
		t.Fatalf("resolver config %s unreadable under landlock: %v\n%s", target, err, out)
	}
	if !strings.HasPrefix(target, "/run/") {
		return
	}
	if _, err := confined(t, root, "ls", "/run"); err == nil {
		t.Fatal("/run should stay unreadable apart from the resolver directory")
	}
}
