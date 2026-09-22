//go:build linux

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"github.com/helpin-ai/agent-runtime/internal/sandbox"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

// Explicitly opt-in: seed may download pinned public packages. Run reuse from
// a replacement pod with the same RWX root and a new ephemeral volume.
func TestRepositoryCacheStaging(t *testing.T) {
	base := os.Getenv("AGENT_RUNTIME_CACHE_SMOKE_ROOT")
	if base == "" {
		t.Skip("set AGENT_RUNTIME_CACHE_SMOKE_ROOT and phase seed/reuse for staging smoke")
	}
	phase := os.Getenv("AGENT_RUNTIME_CACHE_SMOKE_PHASE")
	if phase != "seed" && phase != "reuse" {
		t.Fatal("phase must be seed or reuse")
	}
	if abi, err := sandbox.ABI(); err != nil || abi < 2 {
		t.Fatalf("smoke requires Landlock: ABI=%d error=%v", abi, err)
	}
	t.Setenv(workspace.RepositoryCacheModeEnv, "workspace")
	t.Setenv(workspace.EphemeralRootEnv, filepath.Join(t.TempDir(), "agent-runtime-ephemeral"))
	root := filepath.Join(base, "app", "run", "repositories", "repo-id", "repo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := buildWorker(t)
	sandboxExecutable = func() (string, error) { return binary, nil }
	SetCommandSandbox(CommandSandboxLandlock)
	t.Cleanup(func() { sandboxExecutable = os.Executable; SetCommandSandbox(CommandSandboxNone) })
	env, err := sandboxCommandEnv(root, procenv.Command())
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(dir, program string, args ...string) string {
		t.Helper()
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		commandEnv := env
		if program == "python3" || program == "pip" {
			program, args, commandEnv, err = pythonCommandEnvironment(ctx, root, program, args, env)
			if err != nil {
				t.Fatal(err)
			}
		}
		cwd := filepath.Join(root, dir)
		program, args, err = sandboxCommand(root, cwd, program, args)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.Env, cmd.Dir = commandEnv, cwd
		out, err := cmd.CombinedOutput()
		t.Logf("%s %s: %s\n%s", phase, dir, time.Since(started), out)
		if err != nil {
			t.Fatalf("command failed: %v", err)
		}
		return string(out)
	}

	// Different fixture directories force installation from the retained pnpm
	// store instead of accidentally reusing the previous node_modules directory.
	nodeDir := "node-" + phase
	write(nodeDir+"/package.json", `{"private":true,"dependencies":{"is-number":"7.0.0"}}`)
	nodeArgs := []string{"install", "--ignore-scripts", "--ignore-pnpmfile", "--reporter=append-only"}
	if phase == "reuse" {
		nodeArgs = append(nodeArgs, "--offline")
	}
	run(nodeDir, "pnpm", nodeArgs...)
	run(nodeDir, "node", "-e", "if (!require('is-number')(42)) process.exit(1)")

	write("go/go.mod", "module cache-smoke\n\ngo 1.21\n\nrequire github.com/google/uuid v1.6.0\n")
	write("go/main.go", "package main\nimport (\"fmt\"; \"github.com/google/uuid\")\nfunc main(){fmt.Println(uuid.Nil)}\n")
	if phase == "reuse" {
		env = append(env, "GOPROXY=off", "GOSUMDB=off")
	}
	run("go", "go", "build", "-mod=mod", "-o", "smoke", ".")
	run("go", filepath.Join(root, "go", "smoke"))

	// Debian's Cargo 1.65 uses the entire Git registry index. A pinned small Git
	// dependency tests dependency/build reuse without that unrelated cold cost.
	write("rust/Cargo.toml", "[package]\nname=\"cache-smoke\"\nversion=\"0.1.0\"\nedition=\"2021\"\n[dependencies]\nitoa={git=\"https://github.com/dtolnay/itoa\",rev=\"8ee97f64bf3cbfa42d357a2c39955fd062492e7d\"}\n")
	write("rust/src/main.rs", "fn main(){println!(\"{}\", itoa::Buffer::new().format(42));}\n")
	cargoArgs := []string{"run"}
	if phase == "reuse" {
		cargoArgs = append(cargoArgs, "--offline", "--locked")
	}
	run("rust", "cargo", cargoArgs...)

	if phase == "seed" {
		run(".", "pip", "install", "six==1.17.0")
	}
	run(".", "python3", "-I", "-c", "import six; assert six.__version__ == '1.17.0'; print('retained Python dependency')")
	cache, _, err := workspace.OpenToolCacheRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	for _, path := range []string{"cache/pnpm-store", "cache/pip", "cache/go-build", "go/pkg/mod", "cargo/git", "cargo-target"} {
		entries, err := os.ReadDir(filepath.Join(cache.Name(), path))
		if err != nil || len(entries) == 0 {
			t.Fatalf("empty %s cache: %v", path, err)
		}
	}
	// Run isolation must hold with persistent caches enabled, including writes
	// through a symlink to a sibling run's cache.
	sibling := filepath.Join(base, "app", "other-run", "repositories", "repo-id", "tool-cache")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "outside-cache")
	if err := os.Symlink(sibling, link); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	program, args, err := sandboxCommand(root, root, "touch", []string{filepath.Join(link, "escape-"+phase)})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(program, args...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "Permission denied") {
		t.Fatalf("sibling cache write was not denied: %v %s", err, out)
	}
}
