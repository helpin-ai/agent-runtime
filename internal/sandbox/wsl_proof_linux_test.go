package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// This opt-in test must fail, rather than skip, when the WSL kernel cannot confine.
func TestWSLConfinementProof(t *testing.T) {
	if os.Getenv("SANDBOX_WSL_PROOF") != "1" {
		t.Skip("requires a real WSL2 kernel and Windows fixture")
	}
	abi, err := ABI()
	if err != nil || abi < 3 {
		t.Fatalf("WSL requires Landlock ABI >= 3: abi=%d err=%v", abi, err)
	}
	release, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil || !strings.Contains(strings.ToLower(string(release)), "microsoft") {
		t.Fatalf("not a WSL kernel: %v", err)
	}
	windowsSecret := os.Getenv("SANDBOX_WINDOWS_SECRET")
	windowsExe := os.Getenv("SANDBOX_WINDOWS_EXE")
	if !strings.HasPrefix(windowsSecret, "/mnt/") || !strings.HasPrefix(windowsExe, "/mnt/") {
		t.Fatal("Windows fixture paths are required")
	}
	// The binfmt registration is kernel-wide and may belong to another distro.
	// Test this distro's actual ability to launch Windows code instead.
	config, err := os.ReadFile("/etc/wsl.conf")
	if err != nil || !strings.Contains(string(config), "enabled=false") || !strings.Contains(string(config), "appendWindowsPath=false") {
		t.Fatalf("interop configuration is missing: %v", err)
	}
	if out, err := exec.Command(windowsExe, "/c", "echo", "REX_INTEROP_RAN").CombinedOutput(); err == nil || strings.Contains(string(out), "REX_INTEROP_RAN") {
		t.Fatalf("unconfined Windows interop remains usable: %v %s", err, out)
	}
	if _, err := os.ReadFile(windowsSecret); err != nil {
		t.Fatalf("unconfined Windows fixture unavailable: %v", err)
	}
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	t.Logf("kernel=%s landlock_abi=%d filesystem=%#x workspace=%s", strings.TrimSpace(string(release)), abi, fs.Type, root)
	if out, err := confined(t, root, "sh", "-c", "echo allowed > inside.txt && cat inside.txt"); err != nil || !strings.Contains(out, "allowed") {
		t.Fatalf("allowed workspace write: %v %s", err, out)
	}
	sibling := filepath.Join(base, "secret")
	if err := os.WriteFile(sibling, []byte("fake credential"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(windowsSecret, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sibling, windowsSecret, link} {
		if out, err := confined(t, root, "cat", path); err == nil {
			t.Fatalf("outside read succeeded for %s: %s", path, out)
		}
	}
	for _, path := range []string{sibling + ".written", windowsSecret + ".written"} {
		if out, err := confined(t, root, "sh", "-c", `echo denied > "$1"`, "sh", path); err == nil {
			t.Fatalf("outside write succeeded for %s: %s", path, out)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("outside write changed fixture: %v", err)
		}
	}
	if out, err := confined(t, root, "sh", "-c", `exec "$1" /c echo REX_INTEROP_RAN`, "sh", windowsExe); err == nil || strings.Contains(out, "REX_INTEROP_RAN") {
		t.Fatalf("Windows executable escaped confinement: %v %s", err, out)
	}
}
