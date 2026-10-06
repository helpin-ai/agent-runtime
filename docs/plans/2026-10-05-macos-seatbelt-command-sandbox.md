# macOS Seatbelt command sandbox (plan)

Status: implemented (2026-10-05). This only **adds** a sandbox mode. The Landlock implementation, its defaults and every existing deployment keep their current behavior.

## Why

Commands chosen by an agent (`run_command`, `run_python`, and the Python venv bootstrap) are confined today only by Linux Landlock (`internal/sandbox/landlock_linux.go`). On macOS, `landlock_other.go` fails, so:

- the execution worker cannot use `landlock` there, and `best_effort` runs commands unconfined;
- the lightweight API server never confines commands. Because of that, it does not admit workspace-write or shell tools (`ErrCodingUnavailable` in `internal/engine/execution_policy.go`).

Desktop hosts such as Dost run the lightweight server on a Mac. They need write and shell tools, but only with real confinement. macOS has a kernel-enforced mechanism for this: Seatbelt, through `/usr/bin/sandbox-exec`. Chrome, Bazel and other agent CLIs use it for the same purpose.

## Goals

1. Add an execution isolation mode `seatbelt`. On macOS it confines agent-selected commands the same way Landlock does: full access under the run root (and the trusted state, cache and toolchain paths the Landlock path already computes), with no writes anywhere else.
2. Let the **lightweight API server** admit coding tools only when an explicit `seatbelt` isolation is active, so coding without confinement is never enabled implicitly.
3. Change nothing when `seatbelt` is not selected.

## Non-goals

- Changing Landlock rules, the `landlock-exec` shim, `commandSandboxMode` defaults, or the worker's Linux behavior.
- Restricting network access. Landlock does not restrict it either (that needs ABI 4); parity is kept and can be revisited.
- Confining the local CLI executor. It is unconfined by design (`localCommandKey`), and that stays.
- Process or CPU isolation between runs.

## Current design (for reference)

| Piece | File | Role |
|---|---|---|
| Mode constants and the process-wide mode | `internal/tools/command_sandbox.go` | `landlock`, `best_effort`, `none`; `SetCommandSandbox`, `commandSandboxEnabled` |
| Command rewrite | `sandboxCommandWithCache` (same file) | Re-executes the worker binary as `landlock-exec --root … --cwd … --read-exec … --read-write … -- program args` |
| Shim | `cmd/agent-runtime-worker/landlock_exec.go` | Applies Landlock on the locked thread, then `exec`s the program |
| Ruleset | `internal/sandbox/landlock_linux.go` | Read/exec on system paths, full access on run root plus trusted paths |
| Callers | `workspace_command_tools.go` (`sandboxed := !local && commandSandboxEnabled()`), `python_tools.go` (venv creation) | Wrap commands when a sandbox is enabled |
| Environment | `sandboxCommandEnvWithCache` | Redirects HOME, TMPDIR and tool caches into run-local state, for every non-local command |
| Mode resolution | `commandSandboxMode` in `cmd/agent-runtime-worker/main.go` | Resolves `AGENT_RUNTIME_EXECUTION_ISOLATION` against the kernel |

## Design

### 1. New mode, with separate dispatch

`internal/tools/command_sandbox.go`:

```go
const CommandSandboxSeatbelt = "seatbelt" // new; other constants unchanged

func commandSandboxEnabled() bool {
	mode, _ := commandSandboxMode.Load().(string)
	return mode == CommandSandboxLandlock || mode == CommandSandboxBestEffort || mode == CommandSandboxSeatbelt
}

func sandboxCommandWithCache(root, cwd, program string, args []string, privateCache bool) (string, []string, error) {
	if currentCommandSandbox() == CommandSandboxSeatbelt {
		return seatbeltCommand(root, cwd, program, args, privateCache) // new function, new file
	}
	// existing Landlock body, unchanged
}
```

The Landlock body is not edited; it moves below one new early-return branch. `sandboxCommandEnvWithCache` already runs for every non-local command, so HOME, TMPDIR and caches are redirected into run state in both modes.

### 2. Seatbelt profile and invocation (new files only)

- `internal/sandbox/seatbelt_darwin.go`: `SeatbeltAvailable() error`, `SeatbeltProfile() string`, and `SeatbeltArgs(root, cwd string, readExec, readWrite, denyRead []string) ([]string, error)`.
- `internal/sandbox/seatbelt_other.go`: stubs that return "seatbelt is only available on macOS".
- `internal/tools/command_sandbox_seatbelt.go`: `seatbeltCommand`. It reuses the trusted path computation (`sandboxConfinementRoot`, `ToolStateRoot`, `SharedRepositoryCachePath`, `sandboxToolchainReadExecPaths`) and returns `/usr/bin/sandbox-exec -p <profile> -D …params… -- program args`. No shim and no re-exec are needed, because `sandbox-exec` applies the profile and `exec`s the program itself. The process-group kill on timeout or cancel keeps working unchanged.

Profile (fixed text; every path is passed as a `-D` parameter, never interpolated, so agent input cannot inject rules):

```scheme
(version 1)
(deny default)
(allow process-exec process-fork)
(allow signal (target same-sandbox))
(allow sysctl-read mach-lookup ipc-posix-shm-read-data)
(allow file-read*)                                  ; parity with Landlock's read access to toolchains
(deny file-read* (subpath (param "DENY_READ_0")) …) ; host-supplied secret folders
(allow file-write* (subpath (param "RUN_ROOT")) (subpath (param "RW_0")) …
                   (literal "/dev/null") (literal "/dev/tty") (literal "/dev/dtracehelper"))
(allow network*)
```

Rules that follow from the probe on macOS 26.7 (below):

- **Every path is canonicalized** with `filepath.EvalSymlinks` before it is passed. Seatbelt matches resolved paths, and `/tmp` → `/private/tmp` silently denied writes to an unresolved run root.
- **Read denials.** By default deny `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.config/gh`, `~/.netrc`, `~/Library/Keychains` and `~/Library/Cookies`. Hosts can add paths through `AGENT_RUNTIME_SEATBELT_DENY_READ` (for example their own data folder). Reads stay broad otherwise, matching the current Landlock read posture for toolchains; this is listed under limitations.
- **xcrun cache.** `/usr/bin/python3` and other Xcode shims write a cache under the per-user `DARWIN_USER_TEMP_DIR`, not `TMPDIR`. Allow writes only to `xcrun_db*` there, or accept the harmless warning. This is decided during implementation.

### 3. Mode resolution (opt-in only)

- **Worker** (`cmd/agent-runtime-worker/main.go`, `commandSandboxMode`): add `case "seatbelt"`. On darwin it calls `SeatbeltAvailable()` and fails startup if that fails (fail closed, like `landlock`); on other platforms it is a startup error. The `landlock`, `best_effort` and `none` cases, and the empty default, are unchanged.
- **Lightweight API server** (`cmd/agent-runtime/main.go`): today it never calls `SetCommandSandbox`. Add:

  ```go
  isolation := os.Getenv("AGENT_RUNTIME_EXECUTION_ISOLATION")
  coding := false
  if isolation == "seatbelt" {           // the only value the server acts on
  	if err := sandbox.SeatbeltAvailable(); err != nil { fatal }
  	tools.SetCommandSandbox(tools.CommandSandboxSeatbelt)
  	coding = durableExecutor == nil        // lightweight execution only
  }
  // engine.Config{ …, CodingWorker: coding }
  ```

  Unset, or any other value, leaves the server exactly as it is: no sandbox and no coding tools. Coding admission becomes available **only together with** active confinement.
- **Failure notes.** `sandboxFailureNote` additionally recognises `operation not permitted` (Seatbelt's error text) when the mode is `seatbelt`, so the model learns why a path failed. Landlock output handling is unchanged.
- **Capabilities.** `/v1/capabilities` reports `execution_isolation: "seatbelt"`. This is an additive field.

### 4. What hosts do

Dost would set `AGENT_RUNTIME_EXECUTION_ISOLATION=seatbelt` and `AGENT_RUNTIME_SEATBELT_DENY_READ=<Dost data dir>`. It then enables "Create and edit files" and "Run shell commands", keeps every mutating tool behind approval, and states in the UI that commands are confined to the bot's folder.

## Compatibility checklist

- [ ] `internal/sandbox/landlock_*.go` and `cmd/agent-runtime-worker/landlock_exec.go` are untouched (verified by diff).
- [ ] Existing tests pass unchanged: `command_sandbox_test.go`, `command_sandbox_linux_test.go`, `landlock_exec_test.go`, `landlock_linux_test.go` and the worker `main_test.go` mode table.
- [ ] Linux builds compile no Seatbelt code paths; `seatbelt` on Linux is a startup error.
- [ ] The server with `AGENT_RUNTIME_EXECUTION_ISOLATION` unset still rejects coding tools with `ErrCodingUnavailable`.
- [ ] The local CLI stays unconfined.

## Tests to add

Unit tests (all platforms):
- profile parameters are canonicalized and de-duplicated, and paths with quotes or parentheses cannot alter the profile;
- dispatch chooses Seatbelt only in `seatbelt` mode, and the Landlock argument list is unchanged otherwise (golden test);
- server mode resolution: unset gives no coding; `seatbelt` with Seatbelt unavailable is fatal; `seatbelt` with it available enables coding.

Darwin integration tests (`//go:build darwin`), the counterpart of `command_sandbox_linux_test.go`:
- a write inside the run root succeeds;
- writes to `/tmp`, `$HOME` and another run's folder are denied;
- reads of `~/.ssh` (a temporary fake home) are denied;
- `git`, `python3 -m venv`, `npm` and `go build` work with the redirected caches;
- a timeout kills the whole process group;
- network access works.

CI: add a macOS job for the darwin tests. Linux jobs are unchanged.

## Probe results (macOS 26.7, this repository's host)

Using the profile above through `sandbox-exec -p … -D RUN_ROOT=<resolved path>`:

| Check | Result |
|---|---|
| Write inside the run root | allowed |
| Write to `/tmp/...` or `$HOME` | denied |
| Read `~/.ssh` | denied |
| `python3`, `git` | run (python prints an xcrun cache warning) |
| HTTPS request | allowed |
| Unresolved run root under `/tmp` | writes denied, so canonicalization is required |

## Limitations

- `sandbox-exec` is marked deprecated by Apple but still ships and is widely used; the profile language is undocumented. Tests pin the observed behavior, and the CI job catches OS regressions.
- `mach-lookup` is allowed broadly because many developer tools need system services. A tighter allowlist can follow once real tool usage is measured.
- Reads are broad apart from the deny list, the same trade-off as the Landlock read rules for toolchains. Secrets outside the deny list remain readable.
- Network access is not restricted, the same as with Landlock today.

## Rollout

1. Implement behind the new mode, plus the tests above.
2. Turn it on in Dost (desktop): approval stays on for shell commands, and file writes stay in the bot folder.
3. Optionally offer `seatbelt` to macOS execution workers as well.
