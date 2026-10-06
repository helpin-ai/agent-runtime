package tools

import (
	"fmt"
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
	// CommandSandboxSeatbelt confines commands with macOS Seatbelt
	// (sandbox-exec). It is opt-in and only valid on macOS.
	CommandSandboxSeatbelt = "seatbelt"
)

// sandboxDeniedNote tells the model why a path failed instead of letting it retry.
const sandboxDeniedNote = "Note: commands are confined to this run's workspace and authorized cache paths; other workspaces and caches are not accessible."

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
	mode := currentCommandSandbox()
	return mode == CommandSandboxLandlock || mode == CommandSandboxBestEffort || mode == CommandSandboxSeatbelt
}

func currentCommandSandbox() string {
	mode, _ := commandSandboxMode.Load().(string)
	return mode
}

// CommandSandboxMode reports the process-wide command confinement.
func CommandSandboxMode() string {
	if mode := currentCommandSandbox(); mode != "" {
		return mode
	}
	return CommandSandboxNone
}

// sandboxCommandEnv keeps HOME and temporary state run-local. Opt-in repository
// caches may be shared across authorized runs; mutable environments stay private.
func sandboxCommandEnv(root string, env []string) ([]string, error) {
	return sandboxCommandEnvWithCache(root, env, false)
}

// A private fallback is per-command: never mutate worker-owned cache bindings
// or clear a shared cache another run may still be using.
func sandboxCommandEnvWithCache(root string, env []string, privateCache bool) ([]string, error) {
	state, _, err := runtimeworkspace.ToolStateRoot(root)
	if err != nil {
		return nil, err
	}
	openCache := runtimeworkspace.OpenToolCacheRoot
	if privateCache {
		openCache = runtimeworkspace.OpenPrivateToolCacheRoot
	}
	cache, _, err := openCache(root)
	if err != nil {
		return nil, err
	}
	defer cache.Close()
	for _, name := range []string{"home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(state, name), 0o700); err != nil {
			return nil, err
		}
	}
	overrides := []string{
		"HOME=" + filepath.Join(state, "home"),
		"TMPDIR=" + filepath.Join(state, "tmp"),
	}
	for key, dir := range map[string]string{
		"XDG_CACHE_HOME": "cache", "npm_config_cache": "cache/npm", "npm_config_store_dir": "cache/pnpm-store", "npm_config_cache_dir": "cache/pnpm",
		"YARN_CACHE_FOLDER": "cache/yarn", "PIP_CACHE_DIR": "cache/pip", "UV_CACHE_DIR": "cache/uv", "POETRY_CACHE_DIR": "cache/poetry",
		"GOPATH": "go", "GOCACHE": "cache/go-build", "GOMODCACHE": "go/pkg/mod",
		"CARGO_HOME": "cargo", "CARGO_TARGET_DIR": "cargo-target",
	} {
		if err := cache.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		overrides = append(overrides, key+"="+filepath.Join(cache.Name(), dir))
	}
	if !privateCache && runtimeworkspace.SharedRepositoryCachePath(root) != "" {
		private, _, err := runtimeworkspace.OpenPrivateToolCacheRoot(root)
		if err != nil {
			return nil, err
		}
		defer private.Close()
		for _, dir := range []string{"cargo-home", "cargo-target", "python/poetry-envs", "cache"} {
			if err := private.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
		// Cargo home contains credentials and config, not only downloaded data.
		// Link only dependency directories and their package-manager lock files.
		for _, name := range []string{"git", "registry", ".package-cache", ".package-cache-mutate"} {
			if strings.HasPrefix(name, ".") {
				f, err := cache.OpenFile(filepath.Join("cargo", name), os.O_CREATE|os.O_RDWR, 0o600)
				if err != nil {
					return nil, err
				}
				f.Close()
			} else if err := cache.MkdirAll(filepath.Join("cargo", name), 0o700); err != nil {
				return nil, err
			}
			link := filepath.Join("cargo-home", name)
			target := filepath.Join(cache.Name(), "cargo", name)
			if existing, err := private.Readlink(link); err == nil {
				if existing != target {
					return nil, fmt.Errorf("unexpected Cargo cache link %s", name)
				}
			} else if err := private.Symlink(target, link); err != nil {
				return nil, err
			}
		}
		overrides = append(overrides,
			"CARGO_HOME="+filepath.Join(private.Name(), "cargo-home"),
			// target contains mutable runnable outputs, not just content-addressed
			// compiler inputs. Different branches must not replace each other's binaries.
			"CARGO_TARGET_DIR="+filepath.Join(private.Name(), "cargo-target"),
			"XDG_CACHE_HOME="+filepath.Join(private.Name(), "cache"),
			"POETRY_VIRTUALENVS_PATH="+filepath.Join(private.Name(), "python", "poetry-envs"),
			"npm_config_package_import_method=copy", "npm_config_verify_store_integrity=true", "UV_LINK_MODE=copy")
	}
	if privateCache && runtimeworkspace.SharedRepositoryCachePath(root) != "" {
		// Recovery must populate the same private environment that subsequent
		// normal commands use, even though its download cache is different.
		if err := cache.MkdirAll("python/poetry-envs", 0o700); err != nil {
			return nil, err
		}
		overrides = append(overrides, "POETRY_VIRTUALENVS_PATH="+filepath.Join(cache.Name(), "python", "poetry-envs"))
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
	// The default Go module cache is read-only. These caches are regenerable and
	// removed with their owning workspace or worker, so keep their directories
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
	return sandboxCommandWithCache(root, workingDirectory, program, args, false)
}

func sandboxCommandWithCache(root, workingDirectory, program string, args []string, privateCache bool) (string, []string, error) {
	if currentCommandSandbox() == CommandSandboxSeatbelt {
		return seatbeltCommand(root, program, args, privateCache)
	}
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
	if path := runtimeworkspace.SharedRepositoryCachePath(root); path != "" && !privateCache {
		shimArgs = append(shimArgs, "--read-write", path)
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
	// Seatbelt reports denials as EPERM rather than EACCES.
	if currentCommandSandbox() == CommandSandboxSeatbelt && strings.Contains(strings.ToLower(output), "operation not permitted") {
		return "\n" + sandboxDeniedNote
	}
	return ""
}
