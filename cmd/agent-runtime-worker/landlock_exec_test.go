package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/sandbox"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// TestMain lets the test binary stand in for the worker when re-executed
// with the landlock-exec subcommand.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == landlockExecCommand {
		os.Exit(runLandlockExec(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestParseLandlockExecArgs(t *testing.T) {
	parsed, err := parseLandlockExecArgs([]string{"--root", "/run", "--cwd", "/run/src", "--read-exec", "/toolchain", "--", "go", "build", "-o", "x"})
	if err != nil || parsed.root != "/run" || parsed.cwd != "/run/src" || strings.Join(parsed.readExec, " ") != "/toolchain" || strings.Join(parsed.argv, " ") != "go build -o x" {
		t.Fatalf("unexpected parse: %+v %v", parsed, err)
	}
	for _, args := range [][]string{{}, {"--root", "/run"}, {"--root", "/run", "--cwd", "/run", "--"}, {"--bogus"}} {
		if _, err := parseLandlockExecArgs(args); err == nil {
			t.Fatalf("expected error for %v", args)
		}
	}
}

func TestCommandSandboxMode(t *testing.T) {
	abi, err := sandbox.ABI()
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := commandSandboxMode(false, ""); err != nil || mode != tools.CommandSandboxNone {
		t.Fatalf("shared role: %s %v", mode, err)
	}
	if mode, err := commandSandboxMode(true, "none"); err != nil || mode != tools.CommandSandboxNone {
		t.Fatalf("none: %s %v", mode, err)
	}
	if _, err := commandSandboxMode(true, "sometimes"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	want := tools.CommandSandboxNone
	if abi >= 2 {
		want = tools.CommandSandboxLandlock
	}
	if mode, err := commandSandboxMode(true, "best_effort"); err != nil || mode != want {
		t.Fatalf("best_effort: %s %v", mode, err)
	}
	mode, err := commandSandboxMode(true, "")
	if abi >= 2 && (err != nil || mode != tools.CommandSandboxLandlock) {
		t.Fatalf("default: %s %v", mode, err)
	}
	if abi < 2 && (err == nil || !strings.Contains(err.Error(), "abi")) {
		t.Fatalf("default below abi 2 must fail clearly: %s %v", mode, err)
	}
}

func TestLandlockExecShim(t *testing.T) {
	abi, err := sandbox.ABI()
	if err != nil {
		t.Fatal(err)
	}
	if abi < 2 {
		t.Skipf("landlock abi %d < 2", abi)
	}
	base := t.TempDir()
	root, sibling := filepath.Join(base, "run"), filepath.Join(base, "other")
	for _, dir := range []string{filepath.Join(root, "src"), sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sibling, "secret.txt"), []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := func(args ...string) (string, error) {
		cmd := exec.Command(os.Args[0], append([]string{landlockExecCommand, "--root", root, "--cwd", filepath.Join(root, "src"), "--"}, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SHIM_MARKER=kept"}
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	if output, err := shim("cat", filepath.Join(sibling, "secret.txt")); err == nil || !strings.Contains(strings.ToLower(output), "permission denied") {
		t.Fatalf("sibling read was not denied: %v %q", err, output)
	}
	output, err := shim("sh", "-c", "pwd && echo $SHIM_MARKER > marker.txt && cat marker.txt")
	if err != nil || !strings.Contains(output, filepath.Join(root, "src")) || !strings.Contains(output, "kept") {
		t.Fatalf("shim did not exec with cwd and environment: %v %q", err, output)
	}
	if output, err := shim("definitely-missing-program"); err == nil || !strings.Contains(output, landlockExecCommand) {
		t.Fatalf("missing program should fail through the shim: %v %q", err, output)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 126 {
		t.Fatalf("expected exit 126: %v", err)
	}
}
