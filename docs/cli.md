# Agent Runtime CLI

An original Go / Bubble Tea terminal client for local coding and review. It uses
the existing native Agent Runtime engine, local filesystem tools, and durable
SQLite checkpoints. No Crush application code is included.

## Build and launch

Requires Go 1.25 or newer. Initial supported platforms are Linux and macOS.

```sh
CGO_ENABLED=0 go build -trimpath -o agent-runtime-cli ./cmd/agent-runtime-cli
./agent-runtime-cli --help
./agent-runtime-cli doctor
./agent-runtime-cli
```

The server executable `agent-runtime` retains its existing behavior. Node.js and
npm are not required to run this binary. npm distribution is not published yet.

## Standalone coding

Set one of `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, or `OPENROUTER_API_KEY` in your
shell. The CLI detects configured provider keys, including OpenRouter. Select and save
a preference with `/provider NAME` and `/model MODEL_ID`; explicit runtime
provider/model environment variables override saved preferences. Optional base
URLs come from the existing native provider configuration. Keys are not written to the session DB.

```sh
agent-runtime-cli run --dir ./my-project 'Fix the failing tests and validate the fix'
agent-runtime-cli run --provider openai --model YOUR_MODEL 'Explain this repository'
agent-runtime-cli review --dir ./my-project 'Focus on the current authentication changes'
agent-runtime-cli run --yes --json 'Fix the parser and run its tests'
```

Flags precede the prompt. `--yes` explicitly permits mutating local tools;
otherwise the run pauses for approval. In a terminal, `/approve` approves the
pending action, `/reject` requests changes, and `/quit` exits. Ctrl+C cancels the
active execution and its command process group; press it again when idle to exit. A completed turn
returns to the composer; a new task starts a new run.

Review excludes edit tools and constrains command execution to the runtime's
read-only command policy. Test runners can write caches and artifacts, so running
tests belongs in a coding run. Local commands run with your OS permissions;
workspace path checks and command policy are not an OS sandbox.

The CLI uses the runtime's filtered command environment. PATH, toolchain/cache
configuration, and installed project dependencies remain available. Select extra
project variables explicitly; they are not persisted and must be selected again
when resuming in a new process:

```sh
agent-runtime-cli run --env DATABASE_URL --env PROJECT_MODE 'Run integration tests'
```

## Terminal interface

Interactive mode occupies the alternate screen and restores your terminal on exit.
The header, scrollable transcript, and composer resize with the terminal.
PageUp/PageDown and Ctrl+Home/End navigate the transcript. Use `/help` for commands:
`/models`, `/provider`, `/model`, `/runs`, `/resume ID`, `/details`, `/diff`,
`/connection NAME|local`, `/agents`, `/agent ID`, `/target REF`, and `/new`.
`/details` shows saved events; `/diff` shows current tracked unstaged changes.
Live display buffers are bounded; durable run history remains in SQLite.

## Saved runs

```sh
agent-runtime-cli runs list
agent-runtime-cli runs show RUN_ID
agent-runtime-cli runs logs RUN_ID
agent-runtime-cli runs diff RUN_ID
agent-runtime-cli runs resume RUN_ID --intent approve
agent-runtime-cli runs resume RUN_ID --intent reply 'Use the existing API'
```

Data lives in `os.UserConfigDir()/agent-runtime-cli` (normally
`~/.config/agent-runtime-cli` on Linux). Override with `AGENT_RUNTIME_CLI_HOME`.
The database uses a pure-Go SQLite driver and does not require CGO. Logs go to
`cli.log`; credentials use the OS keyring by default. File and directory modes
are 0600 and 0700 respectively when created.

Sessions retain agent policy snapshots, messages, interactions, tool results,
events, and native checkpoints. A per-workspace OS lock prevents simultaneous
runs using the same CLI data directory from editing the same checkout. A process
crash releases that lock. Queued/running interrupted runs can resume from their
native checkpoint; intentionally cancelled and completed runs are terminal.
Ambiguous tool outcomes are governed by the runtime's existing recovery policy.
After restart, edits requiring an earlier in-memory file observation may be
refused; the agent must re-read the current file rather than apply a stale edit.

`runs diff` displays tracked unstaged Git changes in the current workspace; it is
not a historical per-run patch. The CLI does not automatically create worktrees,
commit, push, or delete existing workspaces.

## Connect an application

A host must implement [CLI host protocol v1alpha1](cli-host-protocol.md). Merely
using the Agent Runtime server or exposing OAuth is not sufficient. There is no
Helpin-specific task, CRM, or authorization logic in the CLI.

```sh
agent-runtime-cli connect https://your-app.example --name work
agent-runtime-cli login work
agent-runtime-cli whoami work
agent-runtime-cli agents work
agent-runtime-cli run --connection work --agent AGENT_ID --target task:123 'Implement this task'
agent-runtime-cli connections
agent-runtime-cli logout work
```

On machines without an OS keyring, explicitly opt into a protected credential
file when creating a connection:

```sh
agent-runtime-cli connect https://your-app.example --name work --credential-store file
```

Login opens the host's browser authorization page and uses PKCE S256 with a
random loopback callback port. Copy the printed URL if a browser cannot open.
Named connections have isolated credentials. Expiring tokens refresh when the
issuer grants a refresh token. Logout revokes the connection at issuers advertising revocation, then deletes
local credentials. If remote revocation fails, credentials are retained for retry.
Legacy issuers without revocation metadata support local-only logout. Device authorization and SSH callback
forwarding are not implemented.

### Helpin admission and connected execution

Helpin now implements the OAuth and admission adapter behind `CLI_ENABLED=false`
by default. Connect to the configured **API origin**, for example
`https://api.helpin.ai` after an administrator deploys and enables the adapter.
Browser consent lets you choose one eligible workspace. Neither Helpin deployment
nor enabling its rollout flag is performed by installing the CLI.

```sh
agent-runtime-cli admit --connection work --agent AGENT_ID --target task:TASK_ID --request-id task-fix-001 'Fix the task'
agent-runtime-cli executions show work HOST_RUN_ID
agent-runtime-cli executions bind work HOST_RUN_ID --epoch 1 --local-run-id LOCAL_RUN_ID
agent-runtime-cli executions renew work HOST_RUN_ID --epoch 1
agent-runtime-cli executions revoke work HOST_RUN_ID
```

Admission creates a normal Helpin run labeled Local and a 15-minute execution
lease without dispatching a cloud worker. Use the same request ID and arguments
to recover a lost admission response. Grant commands return JSON. The request and
response are saved beneath the CLI data directory's `admissions/` folder.
Revoke unused admissions to cancel them and release their usage reservations.
Lease expiry blocks binding until renewal; it does not mark a run completed.

Helpin-connected execution additionally requires `CLI_MODEL_GATEWAY_ENABLED=true`
and the connected-execution database migration. The initial managed gateway accepts
OpenRouter API-key AI profiles; unsupported routes fail admission before a run is
created. Provider credentials stay in Helpin. Existing connections can refresh
advertised capabilities without repeating consent:

```sh
agent-runtime-cli connections refresh work
agent-runtime-cli run --connection work --agent AGENT_ID --target task:TASK_ID --dir ./project 'Fix this task and test it'
```

The CLI binds the execution automatically, renews it every 30 seconds while running,
and reacquires an expired lease when resuming. Loss of the grant cancels local work.
Helpin stores locally reported messages/tool details and event artifacts separately
from provider-observed usage. Tracked unstaged Git patches up to 256 KiB are uploaded
automatically. These are current checkout changes, not isolated per-run patches.
A lost provider response fences further inference to avoid duplicate dispatch;
inspect and revoke that admission before starting another run.

### Hosts with connected execution

The host authorizes the agent/target and returns an immutable admission policy.
The local tool set is intersected with that policy. Model requests go to the
host's authenticated run gateway; provider credentials stay on the host.
Local run history syncs automatically after each execution. If unavailable,
local completion remains saved and sync can be retried:

```sh
agent-runtime-cli runs sync RUN_ID
```

The current protocol supports local coding tools and host model generation.
App-specific remote tools, binary attachments, incremental/background sync,
worktrees, and richer target browsing are follow-up work. Host model responses currently arrive one generation at a time; standalone
providers can stream tokens. Live terminal updates are bounded and may coalesce
under load; saved tool results and durable events remain available.

## Validation

```sh
go test ./internal/cli ./internal/runtime ./internal/engine ./internal/tools
CGO_ENABLED=0 go test ./internal/cli
go test -race ./internal/cli
python3 scripts/cli-smoke.py ./agent-runtime-cli
python3 scripts/cli-terminal-smoke.py ./agent-runtime-cli
```

The CLI integration tests exercise an independent OAuth host, PKCE rejection,
admission/tool intersection, the model gateway, actual Python file edits/tests,
automatic sync, approval across database reopen, review protection, cancellation,
and workspace locking. They use a scripted model and require loopback networking.
The same compiled binary is also exercised against Helpin's real HTTP handlers,
service layer, and a seeded test database. This verifies local protocol integration;
it does not replace deployment, PostgreSQL rollout checks, or live model tests.

To include the compiled-binary admission test:

```sh
AGENT_RUNTIME_CLI_TEST_BINARY=/absolute/path/agent-runtime-cli go test ./internal/cli
# In helpin/server:
AGENT_RUNTIME_CLI_TEST_BINARY=/absolute/path/agent-runtime-cli go test ./internal/service -run '^TestCLI'
```
