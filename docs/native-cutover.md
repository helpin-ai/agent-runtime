# Native-only cutover

This release removes Codex/OpenCode execution and device login. Existing history
remains readable; old runs cannot resume. ChatGPT subscription access uses the
separate, feature-gated native run-credential transport described in
[run credentials](2026-09-12-run-credentials.md). Generic encrypted
integration/MCP credential storage remains in place.

## Gates and inventory

Use the actual production connection profiles, not a developer database. Run
`ops/native-cutover-runtime.sql` against runtime Postgres and Helpin's
`server/scripts/native-cutover-inventory.sql` against Helpin Postgres. Save the
outputs with the release record. They include 30-day usage, every nonterminal
legacy run regardless of age, configurations/versions, scheduled wiring, and
stored Codex connection metadata. Stored rows do not prove valid or active tokens.
No token payloads are selected. A nonzero stored-connection count requires an
explicit API-key migration/disconnect decision before dropping the auth tables.
Validate the separate subscription credential path before enabling its flags.

Before cutover, disable new legacy admission, scheduled starts, and user retries
at the deployment ingress. Keep existing old workers available during a finite
chosen drain. Export the remaining disposition lists using the new operator
binary, with `DATABASE_URL` set by the corresponding secret profile:

```sh
go build -o /tmp/agent-runtime-cutover ./cmd/agent-runtime-cutover
/tmp/agent-runtime-cutover --database runtime > runtime-disposition.json
/tmp/agent-runtime-cutover --database helpin > helpin-disposition.json
```

Review both files. Each row must be drained or explicitly failed. To fail the
listed runs, use the matching database and Temporal profiles (address, namespace,
API key, TLS settings). Apply runtime disposition first, then Helpin projections:

```sh
/tmp/agent-runtime-cutover --database runtime --apply runtime-disposition.json
/tmp/agent-runtime-cutover --database helpin --apply helpin-disposition.json
```

The command terminates old workflows without requiring their queues to be polled,
then records the actionable retirement failure, cancels pending interactions,
clears runtime-scoped MCP credentials, and resets idle host-agent indicators. It refuses newly discovered or
changed workflow identities. It can be rerun after partial failure; already
terminal rows are left intact. For Helpin runs with external runtime IDs, the
runtime disposition owns workflow termination. Legacy Helpin-owned workflows
require their Helpin Temporal profile. Re-export both lists and require zero
nonterminal rows before removing the old worker deployments. Allow active
activities to exit before schema migration; do not race an old deployment's
startup migration against the new schema. Keep the original disposition files
and command output as the concrete audit trail.

## Validation and migration order

1. Checkpoint/recovery and approval/resume tests must pass with compaction off.
   Configure OpenAI API-key access on the native workers.
2. Validate Task Planner, CRM Operator, and Marketer first. Run
   `AGENT_RUNTIME_NATIVE_SMOKE=1 go test ./internal/runtime -run TestNativeNonCodingSmoke -count=1 -v`
   for real API-key tool round trips using fixture host callbacks. They use host business
   tools and repository reads; the shared planner profile no longer grants shell
   execution. Check effective tools and completion interactions after preset
   reconciliation. Saved custom policies with shell/write permissions still need
   coding capacity.
3. Validate Code Builder and Reviewer with the real model smoke gate:
   `AGENT_RUNTIME_NATIVE_SMOKE=1 go test ./internal/runtime -run TestNativeCodingSmoke -count=1 -v`.
   It uses disposable repositories and requires `OPENAI_API_KEY`; set
   `NATIVE_SMOKE_MODEL` to override the configured default. This is a limited
   correctness smoke, not evidence of general coding parity.
4. Build/test the support and coding images. Enable the dedicated coding worker
   only for trusted repositories or an appropriately isolated execution boundary.
   Verify that no coding poller causes admission rejection, default workers never
   poll coding, and a coding run uses `agent-native-coding`.
5. Review runtime-specific imported skill packages from the inventory before
   enabling affected custom agents. Publish/pin a native-compatible package;
   changing a database metadata label does not port an archive's instructions.
   After the zero-nonterminal gate, apply `ops/native-cutover-migrate.sql` on the
   runtime database and Helpin's forward migration
   `20260912000101_native_only_agents.sql` through its normal migration runner.
   It clones selected custom versions, switches defaults, and preserves old run
   and version identities. The preset updates put non-coding presets first,
   followed by coding/custom defaults, in one atomic final migration after both
   sets of gates pass. Automation rules reference agent IDs and follow the migrated active default;
   verify their scheduled wiring from inventory before reopening triggers. New
   admission rejects attempts to select a retired historical agent version.
6. Deploy the coordinated runtime, Helpin server/frontend, and SDK changes.
   Ship the default image from this cutover. Verify old-run history and
   retirement errors, repeat a support workflow, and run a coding turn plus
   approval/resume on the dedicated worker before reopening admission.

### Recorded validation (2026-09-12)

Both real OpenAI API-key smoke gates passed with `gpt-5.6-terra`:

| Gate | Cases | Result |
| --- | --- | --- |
| `TestNativeNonCodingSmoke` | Task Planner, CRM Operator, Marketer | 3/3 passed, 14.18 seconds |
| `TestNativeCodingSmoke` | Patch with verification, multi-file change, known-bug review | 3/3 passed, 52.71 seconds |

The non-coding suite uses fixture host callbacks, not customer records. The coding
suite uses disposable repositories. These results establish the limited smoke
gate, not general coding parity. The [Helpin dev evaluation](2026-09-12-helpin-native-live-evaluation.md)
also records Forge/Lens UI runs, review decisions, and checkpoint/resume after
coding-container recreation. Production inventory has **not** been run: this
environment has no verified production connection profile. Production deployment
remains blocked on both inventory reports, credential disposition, and the
zero-nonterminal legacy-run gate above.

## Coding deployment

Normal workers serve native interactive/autonomous queues and automation.
`--coding` workers serve only `agent-native-coding`, with activity concurrency four per process; `--all-queues` serves every queue from one process for single-tenant installs.
Queue selection comes from resolved shell/workspace-write tool permissions and
is persisted by admission; it ignores user-supplied profile/queue labels. A live
Temporal poller check rejects coding admission when capacity is unavailable.
Both preparation and execution also enforce the policy. The same binary ships in
every image; only the execution image carries the full toolchain.

Helm: set `codingWorker.enabled=true`, use the published `agent-runtime-coding`
image/tag, and configure its node placement for the intended trust boundary.
Kustomize: `k8s/stage` and `k8s/prod` include `../components/coding-worker` and
pin the coding image to the same release as the API. Release jobs publish both
images and update both tags together. Verify the coding worker's storage and
credentials before deploying either environment.

The coding deployment mounts a retained workspace volume at
`/tmp/agent-runtime-workspaces`. Any app-specific repository `root_dir` must
point beneath that mount. Scale coding replicas only on a shared ReadWriteMany
volume: subsequent coding turns must see the same checkout, and execution
workers take a per-run lock on that volume so retries never overlap a stale
writer. On a ReadWriteOnce volume keep one replica with Recreate updates. Missing or invalid
coding continuation workspaces fail clearly instead of being silently recreated.
Repository runs whose effective tools do not require coding can re-prepare a
checkout on another default worker; the replacement lease is saved for resume.
The default coding deployment has one execution slot across all workspaces;
additional coding runs wait for that slot. Paused runs release the execution slot.
Database checkpoints do not snapshot files. The command environment omits
provider/runtime credentials, but a same-user command is not an OS sandbox.

### Linux host development

Starting the API and the normal worker does not enable Forge or Lens. For a
trusted local deployment, `ops/local/compose.coding-worker.yaml` starts a separate
coding container against the host services, with a retained workspace volume and
health endpoint at `127.0.0.1:8092`. It is deliberately opt-in. The coding worker
must share the API's store, Temporal namespace, and task queue prefix, and receive
the same provider and host-tool credentials required by the app configuration.

This example supports the existing local SQLite setup. The release binaries use
`CGO_ENABLED=0` and cannot open SQLite; build a development worker with
`CGO_ENABLED=1 go build -o /private/path/agent-runtime-worker ./cmd/agent-runtime-worker`
using a libc compatible with the Debian coding image. Set these Compose variables
in a private environment file outside the shared state directory:

- `AGENT_RUNTIME_CODING_IMAGE`: the coding image matching the API release.
- `AGENT_RUNTIME_CODING_WORKER_BINARY`: the SQLite-capable worker binary.
- `AGENT_RUNTIME_CODING_ENV_FILE`: private runtime/provider environment file.
- `AGENT_RUNTIME_CODING_APP_CONFIG`: app config JSON; repository `root_dir` must
  be `/tmp/agent-runtime-workspaces` inside the container.
- `AGENT_RUNTIME_CODING_STATE_DIR`: directory containing the API's
  `helpin-agent-runtime.sqlite3`, shared with its journal files.
- `AGENT_RUNTIME_CODING_USER`: UID:GID able to write that SQLite directory
  (default `1000:1000`). Do not mount host repositories or a Docker socket.

Start the container:

```bash
docker compose --project-name helpin-native-coding \
  --env-file /private/path/compose.env \
  -f ops/local/compose.coding-worker.yaml up -d
```

Verify `/readyz` and the coding
queue startup log before launching a new run. Container restart keeps repository
checkouts in the named volume. Rebuild the development binary when updating the
runtime. This host-network/SQLite setup is for trusted development only; use the
Postgres-backed coding deployment above for shared installations.

Repository delivery runs only on successful completion. Paused, failed, and
cancelled runs do not trigger automatic commits or pushes. With the default
`on_terminal` cleanup policy, only paused runs retain their checkout; completed,
failed, and cancelled runs delete it. Preview runs explicitly use `manual`
cleanup as described below.
A completed run with no changes or unpublished commits does not create a remote
branch. Successful runs with changes still use backend-managed delivery; prose
in a task is not a switch that disables that policy.

## Preview runs and command directories

Helpin task launches and manual agent launches accept `delivery_mode: "preview"`
(the existing default is `publish`). Select **Preview changes** before starting
from the task Delivery panel or Run agent now dialog. The run transcript displays
the saved mode. Continuations and server-created child runs inherit preview;
resuming or approving a review cannot turn that run into a publishing run.

Helpin persists the mode in run input and derives the repository spec from that
saved record: `finalize_policy: "none"`, metadata `delivery_mode: "preview"`.
Runtime finalization checks the lease metadata as well. Both Helpin PR finalizers
skip preview runs, even if a stale output summary claims a push. The model cannot
use the registered `commit_and_push` or `open_pr` tools in preview; ordinary Git
commit/push and alias/config command forms are rejected by `run_command`.
Preview is a repository-delivery policy, not an OS/network sandbox or a dry run
for task comments, CRM actions, external MCP tools, or arbitrary child programs.
Coding worker isolation remains required.

Preview checkouts use the existing `manual` cleanup policy, preserving local
changes after completion for inspection. Operators must remove retained preview
checkouts when no longer needed; there is no new archival or retention scheduler.
This does not provide recovery after the workspace volume is lost.

`run_command` accepts optional `working_directory`, relative to the workspace
root, for example `{"program":"python3","args":["-m","unittest"],
"working_directory":"tests"}`. It must be an existing directory; absolute paths,
workspace escapes, files, symlinks, and unknown fields are rejected. Omitting it
continues to run at the root. Runtime and Helpin ship the same tool schema.

## Rollback

Take normal database backups before the final migration. Do not automatically
resume or replay failed old runs after rollback: inspect completed side effects
and start a new run. Removed subscription connections require reconnection if
an old release is restored; encryption primitives remain, but dropped credential
rows are not reconstructed. Keep old run transcripts and audit artifacts.
