package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
	runtimeworkspace "github.com/helpin-ai/agent-runtime/internal/workspace"
)

// Command sandbox modes. The worker resolves best_effort against the kernel at
// startup; here best_effort and landlock both confine.
const (
	CommandSandboxLandlock   = "landlock"
	CommandSandboxBestEffort = "best_effort"
	CommandSandboxNone       = "none"
)

// sandboxDeniedNote tells the model why a path failed instead of letting it retry.
const sandboxDeniedNote = "Note: commands are confined to this run's workspace; paths outside it are not accessible."

var commandSandboxMode atomic.Value

// sandboxExecutable is the binary that carries the landlock-exec subcommand;
// tests point it at a freshly built worker.
var sandboxExecutable = os.Executable
var sandboxLookPath = exec.LookPath

// SetCommandSandbox sets the process-wide confinement for agent-selected
// commands. The local CLI executor is never confined.
func SetCommandSandbox(mode string) {
	commandSandboxMode.Store(strings.TrimSpace(mode))
}

func commandSandboxEnabled() bool {
	mode, _ := commandSandboxMode.Load().(string)
	return mode == CommandSandboxLandlock || mode == CommandSandboxBestEffort
}

// sandboxCommandEnv points regenerable toolchain state at a run-local
// directory. Execution deployments can place that directory on pod-local
// ephemeral storage; other deployments retain the workspace-local layout.
func sandboxCommandEnv(root string, env []string) ([]string, error) {
	state, _, err := runtimeworkspace.ToolStateRoot(root)
	if err != nil {
		return nil, err
	}
	overrides := []string{
		"HOME=" + filepath.Join(state, "home"),
		"TMPDIR=" + filepath.Join(state, "tmp"),
		"XDG_CACHE_HOME=" + filepath.Join(state, "cache"),
		"npm_config_cache=" + filepath.Join(state, "cache", "npm"),
		"npm_config_store_dir=" + filepath.Join(state, "cache", "pnpm-store"),
		"YARN_CACHE_FOLDER=" + filepath.Join(state, "cache", "yarn"),
		"UV_CACHE_DIR=" + filepath.Join(state, "cache", "uv"),
		"POETRY_CACHE_DIR=" + filepath.Join(state, "cache", "poetry"),
		"GOPATH=" + filepath.Join(state, "go"),
		"GOCACHE=" + filepath.Join(state, "cache", "go-build"),
		"GOMODCACHE=" + filepath.Join(state, "go", "pkg", "mod"),
		"CARGO_HOME=" + filepath.Join(state, "cargo"),
		"CARGO_TARGET_DIR=" + filepath.Join(state, "cargo-target"),
	}
	// rustup is a read-only toolchain selector, while Cargo's writable home and
	// package cache remain run-local. Preserve the operator's conventional
	// rustup home so cargo/rustc shims still resolve after HOME is redirected.
	originalHome, rustupHome := "", ""
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			switch key {
			case "HOME":
				originalHome = value
			case "RUSTUP_HOME":
				rustupHome = value
			}
		}
	}
	if rustupHome == "" && originalHome != "" {
		rustupHome = filepath.Join(originalHome, ".rustup")
	}
	if rustupHome != "" {
		if info, err := os.Stat(filepath.Join(rustupHome, "toolchains")); err == nil && info.IsDir() {
			overrides = append(overrides, "RUSTUP_HOME="+rustupHome)
		}
	}
	for _, entry := range overrides {
		_, dir, _ := strings.Cut(entry, "=")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	// The default Go module cache is read-only. These caches are regenerable and
	// removed at run completion or worker startup, so keep their directories
	// writable rather than making cleanup fail and crash-loop the worker.
	overrides = append(overrides, "GOFLAGS=-modcacherw")
	return procenv.SanitizedFrom(env, overrides...), nil
}

// sandboxConfinementRoot widens a repository checkout root to the run's own
// directory so a run with several checkouts can reach all of them. Repository
// leases live at <run dir>/repositories/<fingerprint>/repo; any other root
// (analysis scratch, host-prepared checkouts) is confined as-is.
func sandboxConfinementRoot(root string) string {
	return runtimeworkspace.ConfinementRoot(root)
}

// sandboxCommand rewrites program and args to run through the worker's
// landlock-exec shim, which confines itself to the run directory before exec.
func sandboxCommand(root, workingDirectory, program string, args []string) (string, []string, error) {
	executable, err := sandboxExecutable()
	if err != nil {
		return "", nil, err
	}
	shimArgs := []string{"landlock-exec", "--root", sandboxConfinementRoot(root), "--cwd", workingDirectory}
	if state, ephemeral, err := runtimeworkspace.ToolStateRoot(root); err != nil {
		return "", nil, err
	} else if ephemeral {
		shimArgs = append(shimArgs, "--read-write", state)
	}
	for _, path := range sandboxToolchainReadExecPaths(program) {
		shimArgs = append(shimArgs, "--read-exec", path)
	}
	shimArgs = append(shimArgs, "--", program)
	shimArgs = append(shimArgs, args...)
	return executable, shimArgs, nil
}

// sandboxToolchainReadExecPaths discovers toolchains from the worker-owned
// PATH before Landlock is applied. Absolute agent-selected programs receive no
// extra access: they must already be inside the run or a fixed system path.
func sandboxToolchainReadExecPaths(program string) []string {
	if strings.Contains(program, string(filepath.Separator)) {
		return nil
	}
	resolved, err := sandboxLookPath(program)
	if err != nil {
		return nil
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil
	}
	if sandboxSystemToolchainPath(resolved) {
		return nil
	}

	paths := []string{filepath.Dir(resolved)}
	switch program {
	case "cargo", "rustc":
		cargoHome := filepath.Dir(filepath.Dir(resolved))
		if filepath.Base(cargoHome) == ".cargo" {
			paths = append(paths, filepath.Join(filepath.Dir(cargoHome), ".rustup"))
		}
	case "node", "npm", "npx", "pnpm", "yarn":
		// NVM installs bin/ and lib/ below one version directory. npm and
		// package-manager shims need both, while nothing above that version is
		// exposed.
		paths = append(paths, filepath.Dir(filepath.Dir(resolved)))
	case "go":
		if strings.HasPrefix(resolved, "/snap/bin/") {
			paths = append(paths, "/snap")
		}
	}
	seen := make(map[string]bool, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if path == "." || path == string(filepath.Separator) || seen[path] {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		seen[path] = true
		result = append(result, path)
	}
	return result
}

func sandboxSystemToolchainPath(path string) bool {
	for _, root := range []string{"/usr", "/lib", "/lib64", "/bin", "/sbin", "/etc", "/opt", "/app"} {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func sandboxFailureNote(output string) string {
	if commandSandboxEnabled() && strings.Contains(strings.ToLower(output), "permission denied") {
		return "\n" + sandboxDeniedNote
	}
	return ""
}
