# Phase 0 contracts: durable jobs and run groups

Companion to `2026-11-04-durable-jobs-and-child-agents.md`. Records what phase 0 verified, the integration baseline, and the contracts phase 1a builds against. Where a contract is executable, the test is named.

Date: 2026-11-04.

## 1. Baseline revisions

| Repository | Branch | Commit | Runtime integration |
| --- | --- | --- | --- |
| agent-runtime | `develop` | `a36ced6` (work continues on `feat/run-groups-work-jobs`; `ebf65e3` adds the plan) | — |
| agent-runtime-go | `main` | `2f430a2` = `v0.9.0` (work continues on `feat/run-groups`) | — |
| Rex | `main` | `27a68c0` | runtime `v0.13.19` subprocess, SDK `v0.9.0`, SQLite, lightweight |
| Helpin | `develop` | `5f904d67c` | SDK `v0.7.0` (`server/go.mod:154`), Postgres, Temporal |
| Usermaven | `staging` | `05a8b499c` | Python HTTP/SSE client, no Go SDK |

Baseline test state on the agent-runtime base commit: 12 failures, all environmental and all in `internal/tools` (macOS `/var` → `/private/var` aliasing, missing `rg`, one Python subprocess test). They fail identically on an untouched checkout. Rex and the SDK pass.

## 2. Integration inventory

### Rex

- Runs the runtime as a **subprocess** of `rex-host` (`cmd/rex-host/main.go:342-375`) with `AGENT_RUNTIME_STORE_DRIVER=sqlite`, a plain `runtime.db` path, a second runtime-owned file `memory-jobs.db`, and `AGENT_RUNTIME_RECOVER_INTERRUPTED_RUNS=true`. App config has one app and does not opt into run groups.
- Readiness is `GET /healthz` decoded as `map[string]string`, requiring `status == "ok"`. The runtime only listens after store open and recovery. The supervisor kills and restarts a runtime that is not healthy within 60 s (`runtime_supervisor.go:97-102`), and SIGKILLs 8 s after SIGTERM on stop (`main.go:419-441`), shorter than the runtime's own drain (10 s HTTP + 20 s browsers).
- In-host restarts never overlap (the supervisor reaps the predecessor first). Overlap is possible across hosts: macOS has no single-instance guard (`src-tauri/src/lib.rs:148-150` is Windows-only), and a second host reaps the first host's live runtime through `runtime.pid`; `ReapOrphan` does not wait for exit after SIGKILL; Windows `ReapOrphan` only deletes the pid file.
- Uses `GetRun` polling (700 ms) for run state, not SSE. `checkRun` (`host.go:671-705`) settles a turn on **any status or pause reason it does not know**; `deliver` starts a new run for an unknown non-paused, non-terminal status. The webview sets `run_status` from any event type prefixed `run.` (`src/runStream.ts:123-127`) and ignores unknown SSE names. All stored events are copied into memory-learning evidence.
- Uses `ListRuns` once (`scheduler.go:328`, routine recovery by `host_run_id`); never `SearchRuns`, counts or totals. Reads only `capabilities.execution_isolation`. Does not read run usage.

### Helpin

- One wrapper, `server/internal/service/agent_runtime_client.go`, used by the API and the Temporal worker. Every call sends a bearer token and `?app_id=helpin`. All decoding is lenient (no `DisallowUnknownFields`).
- Calls: `PUT /v1/agents/{id}`, `POST /v1/runs` (durable, or lightweight for the help assistant), `GET /v1/runs/{id}`, resume (SDK and a raw call adding `message_provenance`), pause, cancel, append message, per-run messages/artifacts/interactions/tool-calls, MCP and model credential updates, `GET /v2/runs/{id}/events` replay, `GET /v1/capabilities`.
- Never calls `ListRuns`, `SearchRuns` or run SSE; its lists come from its own projection. Hidden runs and changed totals do not affect it.
- Sends `parent_run_id` **only inside `metadata` / `target.metadata`** (its Dock parent). It never sets top-level `group_id` or `parent_run_id`. The runtime must never read that metadata key as group membership.
- **Events arrive through the runtime event sink** (NATS JetStream subject `agent-runtime.events[.v2].helpin.>`, or the HTTP callback sink), plus `/v2` replay from its reconciliation sweep.
  - An event whose run Helpin cannot resolve (by `host_run_id`, then runtime `run_id`) is NAK'd and retried 5 times over about 7.5 minutes; over the callback sink it gets 500 and is retried.
  - In replay, any apply error stops the cursor.
  - `ParseEventEnvelope` requires `app_id`, `run_id` and `type`.
  - Helpin rewrites the run's `external_runtime_id` from the event's `run_id`, so a child event carrying the parent's `host_run_id` would corrupt the parent mapping.
- Status handling: an unknown run status leaves the Helpin run active indefinitely. An unknown `pause_reason` is treated as `human_input` and triggers attention notifications. Only `run.completed|failed|cancelled` or a `GetRun` terminal status ends a run.
- Usage: takes the max of cumulative values per mapped run. It drives billing settlement and over-budget auto-cancel (`max_billable_microusd`). Usage folded into a parent run would be billed and could cancel it.
- Capabilities: membership checks on `runtime_kinds`, `providers[].name`, `auth_modes`, `protocols`, `run_credentials_configured`, `apps[].model_endpoints`, `apps[].components`. New keys are safe; those keys must not change.
- Deployment: the runtime API and worker pods each run `MigratePostgres` at startup, concurrently; Helpin runs no runtime DDL. Phase 1b DDL must be idempotent and safe to run from several pods at once. Production runtime images are pinned in gitops (`agent-runtime/prod/kustomization.yaml`, currently `v0.13.2`); `DATABASE_URL`, app config, event sink and Temporal settings come from Doppler.
- SDK bump hazard (only if Helpin upgrades past v0.7.0): `sameModelEndpoint` compares `sdk.ModelEndpoint` with `==` (`ai_connection_endpoints.go:55`). Adding a slice or map field to that struct breaks Helpin's build.

### Usermaven

- Uses the **Python SDK** `agent-runtime-python` v0.4.0 (`pyproject.toml:95`, lock commit `491fa40`) through `app/infrastructure/agent_runtime/client.py`. The local Poetry environment still has v0.2.0, so local results can differ from deploys.
- Durable (Temporal) execution for chat and monitors; lightweight for title, suggestions, drafting and the admin model test.
- Calls:
  - start-run (with the `X-Agent-Runtime-Event-Protocol` header);
  - get, resume, cancel;
  - per-run messages, tool-calls, artifacts (GET and POST), interactions;
  - MCP credential PUT;
  - v1 SSE, used only with protocol v2;
  - `/v2/runs/{id}/events` polling;
  - agents GET/PUT/POST;
  - capabilities.
  - Never: `ListRuns`, `SearchRuns`, event history, run groups.
- **Strictness:** pydantic models ignore unknown fields. Required fields still fail the whole call when missing or null:
  - `AgentRun` requires `id, app_id, agent_id, target, runtime_kind, execution_mode, invocation_mode, status, pause_reason (string), approval_state, input`;
  - `ToolCall.output` must be an object or null, and `error` a string or null;
  - `Capabilities` requires `store.driver` and `durable`.
- **Agent PUT replaces the whole agent.** Usermaven re-PUTs only the fields its SDK model knows, so a new server-side agent field resets to its default on every Usermaven save.
- **Callbacks:** `POST /agent-runtime/events` receives every event type (no filter). The envelope requires `event_id, sent_at, sequence_no, app_id, run_id, type`. Terminal events are deduplicated by `event_id`. A callback with a `host_run_id` that does not match the saved runtime ID raises and retries 5 times; while a monitor is still starting, it overwrites the saved runtime ID with the child's.
- **Streams:** the v2 reducer routes by prefix (`assistant.message.*`, `reasoning.message.*`, `tool.call.*`, `run.*`). A new subtype under those prefixes is interpreted, which can create phantom messages or tool rows. A new `run.*`/`run_*` type triggers an extra `get_run`. Duplicates are dropped by sequence number.
- **Status handling:** an unknown status never raises. Chat tasks stay processing and are requeued every 120 s indefinitely. Monitors fail after 10 minutes. Any `paused` monitor fails and is cancelled.
- **Usage:** read only from `output_summary` top-level counters. It requires non-negative ints with `total == input + output`, and charges cumulative deltas per run. Child usage is never queried. The monitor worker fails a run with more than 40 tool calls.
- **MCP tool authorization** (for tools served by Usermaven) needs `external_actor_id`, a resolvable workspace and the run's input metadata.

### Invariants for every additive change

Derived from the inventories; each new feature must keep them for apps that have not opted in, and for host-visible runs even when they have:

1. No new run status and no new `pause_reason` on runs a host started (Rex settles or restarts on unknown values).
2. New event types never use the `run.` prefix (Rex derives run status from it). Group and job events use `run_group.` / `work_job.` and a separate job stream; nothing new is injected into an existing run's SSE stream.
3. `/healthz` stays a string-only JSON object; `status` is `ok` only when ready.
4. New response fields and capability keys are additive and optional; old fields keep their meaning.
5. Default `ListRuns`/`SearchRuns` results and totals change only for runtime-created (hidden) runs, which no current host creates.
6. The unchanged PostgreSQL schema keeps working with the new binary.
7. **Runtime-created (hidden) runs never publish to the app's existing event sink** (NATS subject or HTTP callback) or to `event_callbacks`, unless the app explicitly opts in. Their events are persisted for audit and recovery and are delivered only through the job event stream. Helpin's NATS consumer retries unresolvable events, so leaking them would cause redelivery storms and could stall its projection. This extends plan section 10, which only names `event_callbacks`.
8. Never put a parent's `host_run_id` on a child run or child event, and never write child events into a parent run's event log.
9. Never fold child usage into a parent run's `output_summary` or usage events. Usage stays cumulative and monotonic per run.
10. Group/job membership is read only from the explicit top-level request fields, never from `metadata`. Helpin already sends `metadata.parent_run_id`.
11. Postgres DDL (phase 1b) is idempotent `IF NOT EXISTS`, safe under concurrent startup of several pods.
12. Start-run decoding stays strict and accepts every field hosts send today. New request fields are optional, and fields are never renamed or removed.
13. Run responses always serialize the Python SDK's required `AgentRun` keys, including an empty `pause_reason`.
14. New runtime tools (such as `submit_item_result`) return object outputs and string errors. Their rows exist only on job children, never on a parent's `/tool-calls`.
15. New agent fields must be safe when a host omits them on a whole-agent PUT.
16. New event types never reuse the `assistant.message.`, `reasoning.message.`, `tool.call.` or `run.` prefixes.
17. Child runs inherit the parent's `external_actor_id` and the authorization metadata hosts require for their MCP tools, or they do not get those tools.

**Executable fixtures.** `internal/api/testdata/host_contracts/` holds representative start-run payloads for Usermaven chat and monitor runs, a Helpin durable run (including its `metadata.parent_run_id`) and a Rex turn. Each was built from the host code paths above.
- `TestHostStartRunFixturesDecodeStrictly` decodes each payload strictly, asserts that no group membership is inferred, and posts it through the handler.
- `TestRunResponseKeepsPythonSDKRequiredFields` checks the run response keys.

## 3. Run groups and the atomic completion contract

Implemented and verified on `feat/run-groups-work-jobs` (`f31d2ad` store, `073e3e0` engine/API) and `feat/run-groups` in the SDK (`796d5c7`).

**Terminal transition inventory.** Every terminal write goes through `Store.UpdateRun`:

| Path | Location | Status |
| --- | --- | --- |
| Host/user cancel | `internal/engine/engine.go:630` (`CancelRun`) | cancelled |
| Idle chat completion | `internal/engine/engine.go:1130` (`completeIdleChatRun`) | completed |
| Execution completion | `internal/engine/engine.go:1809` | completed |
| Engine failure | `internal/engine/engine.go:2727` (`failRun`) | failed |
| Temporal failure activity | `internal/durable/activities.go:187` (`markRunFailed`) | failed |

The only other status write in the store is manual pause (`gorm.go:603`), which is non-terminal. `ResumeRun` rejects terminal runs (`engine.go:826`), so terminal is final.

**Single terminal-write operation.** When run groups are migrated and the new status is terminal, `UpdateRun` saves the run and, in the same transaction, increments the group's `last_completion_seq` and writes it to the member row `WHERE completion_seq = 0`. A Postgres `SEQUENCE` is not used because it is not commit-ordered. SQLite's single writer orders commits. Ungrouped runs and stores without group tables keep the old single `Save`.

Tests: `internal/store/run_groups_test.go` (both stores; 48 goroutines racing completion against cancel produce gap-free sequences 1–24 on file-backed SQLite; legacy store without tables), `internal/engine/run_groups_test.go`, `internal/api/run_groups_test.go`.

**Host consumption contract.** Completions are pulled with `GET /v1/run-groups/{id}/completions?after=<seq>&limit=<n>` (limit ≤ 500, ordered by sequence, at-least-once). Push callbacks and SSE are wake-up hints only. A host that advances its own state from completions must:

1. In one host database transaction, record the consumed `(group_id, seq)` cursor and durably apply (or durably enqueue) the resulting step. Idempotency key: `(group_id, seq)`; a replay of an applied sequence is a no-op.
2. Treat a crash before commit as "not consumed": the next pull returns the same completion.
3. Make any backstop sweeper take the same row lock or idempotency key as the consumer, so a sweep racing a completion cannot advance a step twice.
4. Never infer completion from a final assistant message or from callback delivery.

This is a requirement on the Helpin pilot (phase 1b); Rex consumes completions through the job module, which follows the same rule internally.

## 4. Usage watermark

Contract: the persisted native checkpoint's cumulative usage (`runtime.NativeCheckpointUsage`) is the only accounting input for job budgets. It is monotonic; repeated reads are idempotent; replaying a completed checkpoint adds nothing; resume takes `max(run summary, checkpoint)` and adds only new provider work; a provider call that fails after billing still advances it. `usage.checkpoint` events are hints and are never summed.

Tests: `internal/runtime/native_usage_watermark_test.go` (new), plus existing `TestCumulativeOutputSummaryDoesNotAddNativeCheckpointTwice`, `TestCumulativeOutputSummaryAddsNativeResumeUsage`, `TestNativeContextInterruptedStreamUsagePersistsAfterCancellation`, `TestNativeContextFailedProviderRetainsUsage`. All pass on the unchanged runtime: no accounting change is needed. Only `native_sdk` runs have this watermark, so initial job children must be native.

The job applies the delta between a child's current watermark and the per-run watermark it last recorded, in the same transaction that records the new watermark.

## 5. Batch-stops-early baseline

`internal/runtime/native_batch_stops_early_test.go` records today's behavior: a run told to handle ten items records three and finishes. With implicit completion it ends on plain text; with explicit completion `finish_turn(completed)` is accepted. The runtime has no item knowledge, so seven items vanish silently. Job children must not inherit this. Their batch outcome is derived from accepted item results (section 6). Ordinary runs keep the current behavior.

## 6. Phase 1a specifications

### Exclusive child recovery — implemented

`RecoverInterruptedLightweightRuns` skips members of work-job groups regardless of opt-in, and skips a run whose membership lookup fails rather than risk executing it twice. Host-group members stay under generic recovery. Test: `TestRecoverInterruptedLightweightRunsSkipsWorkJobWorkers`.

### Main SQLite process-owner lock

- Lock file `<canonical db path>.runtime-owner.lock` via `internal/filelock`. Identity normalizes plain paths, `file:` URIs and query parameters to one absolute path. `:memory:` and `mode=memory` are not lockable and cannot enable jobs.
- Acquire before migrations, recovery or admission, for every SQLite file store (not only when jobs are enabled, so an old/new overlap is excluded for ordinary recovery too). The memory-jobs database is covered by the same owner, since only the owner process opens it.
- **Serve health early.** While waiting, the HTTP server listens and `/healthz` returns `{"status":"waiting_for_store_owner","owner":"<pid or unknown>"}` (strings only). All other routes return 503. Rex today treats non-`ok` as not ready and kills after 60 s, so Rex must change in the same release (below).
- Retry with cancellable backoff 250 ms → 2 s, rate-limited logs. Never unlink, steal or kill.
- Shutdown order: stop admission → cancel/drain executions → close store → release lock. A crash releases the OS lock; the file's existence is not ownership.
- Older binaries ignore the lock. Rex must confirm an old runtime has exited before starting a new one; the lock is not claimed to fence old software.
- **Rex changes:**
  - The supervisor recognizes `waiting_for_store_owner` as a distinct state, does not apply `readyTimeout` or restart while in it, and shows "Waiting for the previous engine to exit" in place of "Starting the bot engine…".
  - Add a macOS single-instance guard so a second Rex cannot reap the first one's runtime.
  - Make `ReapOrphan` wait for exit after SIGKILL.
  - Raise the stop grace period to cover the runtime drain.
- Tests: two processes on one file (second waits, then acquires after the first exits); aliases map to one lock; cancellation while waiting; `/healthz` body during waiting; crash release.

### `submit_item_result` receipts and fencing

The specification is in plan section 6. Executable cases for phase 1a:
1. Accepted: returns a receipt `{item_id, result_revision, status: accepted}` only after commit.
2. Identical replay (same attempt, same result digest): returns `already_accepted` with no write.
3. Different content for an accepted item: rejected.
4. An item not assigned to this attempt: rejected.
5. Stale execution generation (after recovery granted a newer one): rejected.
6. Validation failure: returns field errors; the item stays unresolved.
7. The tool is absent from non-job runs, and an MCP tool cannot shadow the name.

Generation source: `agent_run_group_members.execution_generation`, bumped by `BumpRunExecutionGeneration` at dispatch, recovery and resume.

### Child completion

For job children only: `finish_turn(completed)` with unresolved assigned items ends the attempt as `partial` (not success), keeps accepted items and returns the unresolved ones to bounded retry. All items accepted makes the batch successful; the job controller still owns final verification. Tests: invert the batch-stops-early baseline for a job child, and keep it unchanged for an ordinary run.

### Event isolation for hidden runs

- Today every event goes through `engine.MultiEventSink{PersistedEventSink, globalEventSink, appEventSink}` (`cmd/agent-runtime/main.go:129-133`). `globalEventSink` is the NATS or callback sink from `OpenEventSinkFromEnv`; `appEventSink` is per-app `event_callbacks`.
- Phase 1a puts a visibility filter in front of the global and app sinks. Events of hidden run-group members (work-job workers) reach only the persisted sink and the job event stream. The persisted sink stays first and unfiltered.
- Host-group members and ungrouped runs are unaffected.
- The filter decides by membership. It caches the hidden flag per run, since membership is immutable after creation, so the hot path does not query the store per delta.

### Memory store

Work jobs report `unsupported_store` on `driver=memory` and on in-memory SQLite. Run groups remain available on the memory store, for contract parity and tests only. Job and recovery tests use file-backed SQLite.

### Capability-first SDK

Clients read `capabilities.run_groups` / `capabilities.work_jobs` for the requesting app. A nil field means an old runtime, so the feature is unavailable. `supported:false` carries a reason (`unsupported_store`, `app_disabled`). A 404 from a feature route is only a fallback "not available" and never distinguishes an old runtime from a disabled app. Tests: SDK `run_groups_test.go` (old capabilities decode with nil, 404 surfaces as `HTTPStatusError`), runtime `TestRunGroupsCapabilityIsPerApp`.

## 7. Fixed decisions → fixtures

| Decision | Fixture | State |
| --- | --- | --- |
| Run groups independent of jobs, per-app opt-in, 404 for disabled apps | `TestRunGroupsDisabledPreservesLegacyRunContract`, `TestRunGroupsCapabilityIsPerApp` | passing |
| Visibility by owner kind; internal scans inclusive | store run-group tests, `TestRunGroupsAPIEndToEnd` | passing |
| Atomic completion sequence | store concurrency test | passing |
| Unchanged Postgres schema with the new binary | `TestPostgresLegacySchemaUnchangedByRunGroups` (set `AGENT_RUNTIME_TEST_POSTGRES_DSN`); binary upgrade smoke (below) | passing |
| Usage watermark | section 4 | passing |
| Exclusive recovery | section 6 | passing |
| Main-store ownership (SQLite lock) | section 6 | phase 1a |
| Batched item acceptance, receipts, fencing | section 6 | phase 1a |
| Event isolation for hidden members | dispatch a job child with a broad `event_callbacks` filter and a recording app event sink (NATS/callback). Assert no delivery to either, events persisted, and none with a parent `host_run_id`. Host-group members still deliver. | phase 1a (no hidden runs exist before the job module) |
| Runtime validation and publisher ownership | plan sections 7–8 | phase 1a |
| Database-authoritative Temporal driver | plan section 4 | phase 1b |

**Postgres upgrade smoke** (real binaries, Postgres 16): the base `develop` binary migrated a fresh database and completed a run. The new binary then started on the same database and served the old run, listing, search, event history and `host_run_id` idempotency unchanged. It completed and cancelled a new run, reported `run_groups: {supported:false, reason:unsupported_store}` for an opted-in app, and returned 404 for group routes and grouped start. It created no group tables and logged no errors.

## 8. School-research contract (draft for the pilot)

**Input item:**

```json
{"item_id": "nces-123456", "name": "Example High School", "city": "Springfield", "state": "IL", "website": "https://example.k12.il.us"}
```

`item_id` is stable and unique within the job; `website` is optional.

**Output per item (`submit_item_result.result`):**

```json
{
  "outcome": "found | not_found | exception",
  "school_website": "https://...",
  "forums": [
    {"url": "https://...", "platform": "discourse | facebook_group | reddit | school_portal | other", "audience": "parents | students | alumni | staff | community", "access": "public | login_required", "last_activity": "YYYY-MM-DD or null", "evidence": {"source_url": "https://...", "excerpt": "≤ 300 chars quoted from the page", "retrieved_at": "RFC 3339"}}
  ],
  "searched": ["queries or URLs checked"],
  "exception": {"kind": "site_unreachable | blocked | ambiguous_school | needs_login | other", "detail": "..."},
  "notes": "optional"
}
```

**Deterministic validation (runtime, phase 1a):**
- schema valid;
- `found` has ≥ 1 forum, each with `evidence.source_url` and a non-empty excerpt;
- `not_found` has ≥ 3 `searched` entries and includes the school website when it is known;
- `exception` has `kind` and `detail`;
- URLs are absolute http(s);
- the item is assigned to this attempt.

**Exception policy:**
- `site_unreachable` and `blocked` are retryable up to 2 more attempts. After that the item is accepted as an exception and never becomes `not_found`. An unreachable site is not evidence of absence.
- `ambiguous_school` and `needs_login` are accepted as exceptions on the first report and listed for the user.
- The job completes when every item is `found`, `not_found` or an accepted exception, and accepted exceptions are ≤ 10% of items. Above that threshold the job blocks for a user decision rather than completing.

**Quality review (pilot only, human):**
- sample 20 `found` and 10 `not_found` items;
- check the forum exists and the excerpt is on the page;
- check `not_found` against a manual search.

Target: ≥ 90% precision on `found`, ≤ 10% false `not_found`.

## 9. Pilot (open)

Run 50–100 items with the contract above, in batches of 10, through Rex with a real model and a throwaway re-prompt loop. Record:
- per-item outcome mix;
- validation failures by field;
- items silently skipped by the model before re-prompt;
- tokens and cost per item;
- wall-clock time per batch;
- the human quality review above.

This needs an owner decision on who runs it and which model to use, and it hits live websites.

### Pilot results (2026-11-04)

Browser off, first 10 schools: 10/10 results valid in the first reply, none skipped, no validation failures, 3.5 minutes, about 16k tokens per item. 8 found and 2 not found, but 6 of the 8 finds were single-class reunion groups (for example a 9-member "Class of 67" group): the excerpts were real, the relevance definition was missing.

Browser on, 60 schools: 5 of 6 batches stopped with a `request_user_input` question ("take over the browser to sign in, or finish with public sources?") instead of reporting `needs_login`; about 30k tokens per item.

Consequences, applied in phase 1a: the worker contract tells workers that an obstacle affecting one item is that item's exception, not a question for the user; job completion is decided by accepted items, not by the worker. Still open for the school contract: a relevance rule (school-wide rather than one graduating class, recent activity, parents or students as audience).
