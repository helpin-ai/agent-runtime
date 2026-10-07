package tools

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSeatbeltModeRoutesThroughSandboxExec(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_EPHEMERAL_ROOT", "")
	SetCommandSandbox(CommandSandboxSeatbelt)
	t.Cleanup(func() { SetCommandSandbox(CommandSandboxNone) })
	root := t.TempDir()
	program, args, err := sandboxCommand(root, root, "go", []string{"build", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	if program != "/usr/bin/sandbox-exec" || args[0] != "-p" {
		t.Fatalf("seatbelt mode must run sandbox-exec: %s %v", program, args)
	}
	if !strings.HasSuffix(strings.Join(args, " "), "-- go build ./...") {
		t.Fatalf("program and args must be preserved: %v", args)
	}
	if !commandSandboxEnabled() || CommandSandboxMode() != CommandSandboxSeatbelt {
		t.Fatal("seatbelt must count as an enabled sandbox")
	}
}

func TestLandlockShimUnchangedOutsideSeatbeltMode(t *testing.T) {
	for _, mode := range []string{CommandSandboxNone, CommandSandboxLandlock, CommandSandboxBestEffort} {
		SetCommandSandbox(mode)
		sandboxExecutable = func() (string, error) { return "/app/agent-runtime-worker", nil }
		sandboxLookPath = func(string) (string, error) { return "/usr/bin/go", nil }
		program, args, err := sandboxCommand("/work/run", "/work/run/src", "go", []string{"build"})
		sandboxExecutable, sandboxLookPath = os.Executable, exec.LookPath
		if err != nil {
			t.Fatal(err)
		}
		want := "landlock-exec --root /work/run --cwd /work/run/src -- go build"
		if program != "/app/agent-runtime-worker" || strings.Join(args, " ") != want {
			t.Fatalf("mode %q changed the landlock invocation: %s %v", mode, program, args)
		}
	}
	SetCommandSandbox(CommandSandboxNone)
}

func TestSeatbeltDenyReadIncludesHostPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(SeatbeltDenyReadEnv, "/data/host-app"+string(os.PathListSeparator)+" ")
	paths := strings.Join(seatbeltDenyRead(), "\n")
	for _, want := range []string{home + "/.ssh", home + "/Library/Keychains", "/data/host-app"} {
		if !strings.Contains(paths, want) {
			t.Fatalf("missing deny-read path %s in %s", want, paths)
		}
	}
}

func TestSeatbeltFailureNote(t *testing.T) {
	SetCommandSandbox(CommandSandboxSeatbelt)
	t.Cleanup(func() { SetCommandSandbox(CommandSandboxNone) })
	if sandboxFailureNote("sh: x: Operation not permitted") == "" {
		t.Fatal("seatbelt denials should explain the workspace confinement")
	}
	SetCommandSandbox(CommandSandboxLandlock)
	if sandboxFailureNote("sh: x: Operation not permitted") != "" {
		t.Fatal("landlock output handling must be unchanged")
	}
}
