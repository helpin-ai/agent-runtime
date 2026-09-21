package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxCommandEnvRedirectsToolchainsUnderRunRoot(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_EPHEMERAL_ROOT", "")
	root := t.TempDir()
	env, err := sandboxCommandEnv(root, []string{"PATH=/usr/bin", "HOME=/root", "GOCACHE=/root/.cache/go-build", "DATABASE_URL=secret"})
	if err != nil {
		t.Fatal(err)
	}
	joined := "\n" + strings.Join(env, "\n") + "\n"
	state := filepath.Join(root, ".agent-runtime")
	for key, dir := range map[string]string{"HOME": "home", "TMPDIR": "tmp", "XDG_CACHE_HOME": "cache", "npm_config_cache": "cache/npm", "npm_config_store_dir": "cache/pnpm-store", "YARN_CACHE_FOLDER": "cache/yarn", "UV_CACHE_DIR": "cache/uv", "POETRY_CACHE_DIR": "cache/poetry", "GOPATH": "go", "GOCACHE": "cache/go-build", "GOMODCACHE": "go/pkg/mod", "CARGO_HOME": "cargo", "CARGO_TARGET_DIR": "cargo-target"} {
		want := filepath.Join(state, dir)
		if !strings.Contains(joined, "\n"+key+"="+want+"\n") {
			t.Fatalf("%s not redirected: %s", key, joined)
		}
		if info, err := os.Stat(want); err != nil || !info.IsDir() {
			t.Fatalf("%s was not created: %v", want, err)
		}
	}
	if !strings.Contains(joined, "\nPATH=/usr/bin\n") || strings.Contains(joined, "DATABASE_URL") || strings.Contains(joined, "HOME=/root\n") {
		t.Fatalf("unexpected environment: %s", joined)
	}
}

func TestSandboxCommandEnvPrecedesPythonEnvironment(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_EPHEMERAL_ROOT", "")
	root := t.TempDir()
	env, err := sandboxCommandEnv(root, []string{"PATH=/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	// The Python environment applies after the generic redirects; its private
	// HOME, TMPDIR and PIP_CACHE_DIR must win.
	state := filepath.Join(root, ".agent-runtime", "python")
	env = append(env, "HOME="+filepath.Join(state, "home"), "TMPDIR="+filepath.Join(state, "tmp"))
	last := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		last[key] = value
	}
	if last["HOME"] != filepath.Join(state, "home") || last["TMPDIR"] != filepath.Join(state, "tmp") {
		t.Fatalf("python environment did not override generic redirects: %v", last)
	}
}

func TestSandboxCommandUsesEphemeralStateAsTrustedWritablePath(t *testing.T) {
	base := filepath.Join(t.TempDir(), "agent-runtime-ephemeral")
	t.Setenv("AGENT_RUNTIME_EPHEMERAL_ROOT", base)
	root := t.TempDir()
	env, err := sandboxCommandEnv(root, []string{"PATH=/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	var home string
	for _, entry := range env {
		if strings.HasPrefix(entry, "HOME=") {
			home = strings.TrimPrefix(entry, "HOME=")
		}
	}
	if !strings.HasPrefix(home, base+string(filepath.Separator)) {
		t.Fatalf("HOME not placed on ephemeral storage: %q", home)
	}
	sandboxExecutable = func() (string, error) { return "/app/agent-runtime-worker", nil }
	t.Cleanup(func() { sandboxExecutable = os.Executable })
	_, args, err := sandboxCommand(root, root, "python3", []string{"-c", "pass"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	state := filepath.Dir(home)
	if !strings.Contains(joined, "--read-write "+state) {
		t.Fatalf("ephemeral state was not granted to the shim: %v", args)
	}
}

func TestSandboxCommandRoutesThroughShim(t *testing.T) {
	sandboxExecutable = func() (string, error) { return "/app/agent-runtime-worker", nil }
	sandboxLookPath = func(string) (string, error) { return "/usr/bin/go", nil }
	t.Cleanup(func() { sandboxExecutable = os.Executable; sandboxLookPath = exec.LookPath })
	program, args, err := sandboxCommand("/work/run", "/work/run/src", "go", []string{"build", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"landlock-exec", "--root", "/work/run", "--cwd", "/work/run/src", "--", "go", "build", "./..."}
	if program != "/app/agent-runtime-worker" || strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("unexpected shim invocation: %s %v", program, args)
	}
}

func TestSandboxCommandAllowsOnlyDiscoveredRustToolchainPaths(t *testing.T) {
	home := t.TempDir()
	cargoBin := filepath.Join(home, ".cargo", "bin")
	rustup := filepath.Join(home, ".rustup")
	for _, dir := range []string{cargoBin, filepath.Join(rustup, "toolchains")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cargo := filepath.Join(cargoBin, "cargo")
	if err := os.WriteFile(cargo, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sandboxExecutable = func() (string, error) { return "/app/agent-runtime-worker", nil }
	sandboxLookPath = func(program string) (string, error) { return cargo, nil }
	t.Cleanup(func() { sandboxExecutable = os.Executable; sandboxLookPath = exec.LookPath })

	_, args, err := sandboxCommand("/work/run", "/work/run", "cargo", []string{"test"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, path := range []string{cargoBin, rustup} {
		if !strings.Contains(joined, "--read-exec "+path) {
			t.Fatalf("missing toolchain path %q in %v", path, args)
		}
	}
	if paths := sandboxToolchainReadExecPaths(filepath.Join(home, "agent-selected", "cargo")); len(paths) != 0 {
		t.Fatalf("absolute agent program gained access: %v", paths)
	}
}

func TestSandboxFailureNoteOnlyWhenConfined(t *testing.T) {
	SetCommandSandbox(CommandSandboxNone)
	t.Cleanup(func() { SetCommandSandbox(CommandSandboxNone) })
	if sandboxFailureNote("cat: /etc/shadow: Permission denied") != "" {
		t.Fatal("note added while unconfined")
	}
	SetCommandSandbox(CommandSandboxLandlock)
	if !strings.Contains(sandboxFailureNote("cat: /x: Permission denied"), sandboxDeniedNote) || sandboxFailureNote("exit status 1") != "" {
		t.Fatal("note does not follow permission denied output")
	}
}

func TestSandboxConfinementRootWidensToRunDirectory(t *testing.T) {
	run := filepath.Join("/tmp/agent-runtime-workspaces", "app-a", "run-1")
	repo := filepath.Join(run, "repositories", "abc123", "repo")
	if got := sandboxConfinementRoot(repo); got != run {
		t.Fatalf("repository checkout should confine to the run directory, got %q", got)
	}
	scratch := "/tmp/agent-runtime-workspaces/analysis-deadbeef"
	if got := sandboxConfinementRoot(scratch); got != scratch {
		t.Fatalf("analysis scratch should confine to itself, got %q", got)
	}
	if got := sandboxConfinementRoot("/repo"); got != "/repo" {
		t.Fatalf("a bare checkout should confine to itself, got %q", got)
	}
}
