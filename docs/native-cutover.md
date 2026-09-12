# Native-only cutover

This release removes Codex/OpenCode execution and device login. Existing history
remains readable; old runs cannot resume. ChatGPT subscription transport is
excluded. Generic encrypted integration/MCP credential storage remains in place.

## Gates and inventory

Use the actual production connection profiles, not a developer database. Run
`ops/native-cutover-runtime.sql` against runtime Postgres and Helpin's
`server/scripts/native-cutover-inventory.sql` against Helpin Postgres. Save the
outputs with the release record. They include 30-day usage, every nonterminal
legacy run regardless of age, configurations/versions, scheduled wiring, and
stored Codex connection metadata. Stored rows do not prove valid or active tokens.
No token payloads are selected. A nonzero stored-connection count requires an
explicit API-key migration/disconnect decision before dropping the auth tables.
Subscription-provider work remains a separately approved follow-up.

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
   Verify that no coding poller causes admission rejection, support workers never
   poll coding, and a coding run uses `agent-native-coding`.
5. Review runtime-specific imported skill packages from the inventory before
   enabling affected custom agents. Publish/pin a native-compatible package;
   changing a database metadata label does not port an archive's instructions.
   After the zero-nonterminal gate, apply `ops/native-cutover-migrate.sql` on the
   runtime database and Helpin's forward migration
   `202609120001_native_only_agents.sql` through its normal migration runner.
   It clones selected custom versions, switches defaults, and preserves old run
   and version identities. The preset updates put non-coding presets first,
   followed by coding/custom defaults, in one atomic final migration after both
   sets of gates pass. Automation rules reference agent IDs and follow the migrated active default;
   verify their scheduled wiring from inventory before reopening triggers. New
   admission rejects attempts to select a retired historical agent version.
6. Deploy the coordinated runtime, Helpin server/frontend, and SDK changes.
   Ship the lean support image from this cutover. Verify old-run history and
   retirement errors, repeat a support workflow, and run a coding turn plus
   approval/resume on the dedicated worker before reopening admission.

## Coding deployment

Normal workers serve native interactive/autonomous queues and automation.
`--coding` workers serve only `agent-native-coding`, with activity concurrency 1.
Queue selection comes from resolved shell/workspace-write tool permissions and
is persisted by admission; it ignores user-supplied profile/queue labels. A live
Temporal poller check rejects coding admission when capacity is unavailable.
Both preparation and execution also enforce the policy. The support binary is
built with coding disabled and refuses `--coding`.

Helm: set `codingWorker.enabled=true`, use the published `agent-runtime-coding`
image/tag, and configure its node placement for the intended trust boundary.
Kustomize: opt into `../components/coding-worker` from `k8s/stage` or `k8s/prod`
with `components`, and set the coding image to the same release tag. The component
is deliberately absent from the default support-only manifests. Release jobs
publish both images and update the coding tag when the component is enabled.

The coding deployment uses one replica, Recreate updates, and a retained 20Gi
PVC at `/tmp/agent-runtime-workspaces`. Any app-specific repository `root_dir`
must point beneath that mount. Do not scale coding replicas onto independent
volumes: subsequent turns must see the same checkout. Missing or invalid
continuation workspaces fail clearly instead of being silently recreated.
Database checkpoints do not snapshot files. The command environment omits
provider/runtime credentials, but a same-user command is not an OS sandbox.

## Rollback

Take normal database backups before the final migration. Do not automatically
resume or replay failed old runs after rollback: inspect completed side effects
and start a new run. Removed subscription connections require reconnection if
an old release is restored; encryption primitives remain, but dropped credential
rows are not reconstructed. Keep old run transcripts and audit artifacts.
