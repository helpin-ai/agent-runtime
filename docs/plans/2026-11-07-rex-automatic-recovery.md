# Rex automatic recovery after connection loss and sleep

Status: original design. The initial implementation uses existing running-state
metadata and execution ownership; see [Automatic connection recovery](../automatic-recovery.md)
for the implemented contract and remaining extensions.

Rex should keep accepted work pending when a temporary connection failure,
provider outage, computer sleep, or process restart interrupts execution. Chats,
scheduled routines, and jobs should continue automatically from saved progress
when their dependencies become available. Users should not have to send “try
again” to restart work that Rex already accepted.

The first delivery targets Rex's native lightweight execution on macOS and
Windows. Implement the execution behavior in agent-runtime, expose its contract
through agent-runtime-go, and integrate admission, schedules, desktop lifecycle,
and presentation in Rex. Enable it explicitly for Rex; other runtime hosts and
Temporal workflows retain their current behavior until separately enabled and
tested. No new queue service or orchestration dependency is needed.

## Product behavior

| Situation | Required behavior |
| --- | --- |
| Connection drops before the first model response | Keep the accepted turn pending and retry automatically. |
| Connection drops after several completed steps | Resume the same run and turn from its checkpoint. Preserve completed tools and results. |
| Provider returns a temporary rate limit or overload | Wait with backoff, honor the provider's retry time, and retry automatically. |
| Computer sleeps during execution | Reconcile after wake; continue an existing healthy execution or recover the interrupted execution, without starting a second copy. |
| Schedule becomes due while asleep, offline, or Rex is closed | On wake/startup, start the latest unstarted due occurrence once when execution is available. |
| Rex or its runtime restarts during a wait | Reload the persisted wait and continue recovery. |
| A job worker loses connectivity | Keep its attempt and assigned items open; resume its checkpoint. Accepted item results remain accepted. |
| A local model still works while internet access is unavailable | Continue using it; internet connectivity is not a global execution gate. |
| User cancels, pauses a job, or disables a routine | Automatic recovery respects that control and cannot silently undo it. |
| Credentials, quota, approval, or task input require a decision | Show the specific reason and keep the existing user action flow. |
| A mutating tool may have succeeded but its response was lost | Reconcile its outcome before another mutation; do not automatically repeat the action. |

“Automatically” means while the computer is awake and Rex's host/runtime are
running. Work missed while Rex is closed is recovered at the next launch. This
does not require waking the computer or launching Rex at login.

Multiple missed schedule occurrences are coalesced into the latest occurrence.
An already started occurrence retains its identity and is resumed first; it is
not silently replaced by a newer occurrence. If further slots have become due,
retain at most the latest unstarted slot and run it after the active occurrence.

## Existing behavior and gaps

Paths below are relative to the named repository.

| Repository and location | Existing behavior | Gap |
| --- | --- | --- |
| Rex `internal/rex/host.go`, `retryTransientFailure` | Matches error strings and starts one fresh run after five seconds, only when no tool ran in the turn. | Cannot recover the reported multi-step runs, survive a long outage, or preserve the same execution identity. |
| Rex `internal/rex/host.go`, `checkRun` and `recoverTurns` | Settles failed turns and reattaches open turns after startup; turns without a run ID are failed. | Needs to retain automatic waits and reconcile accepted but incompletely dispatched turns. |
| Rex `internal/rex/scheduler.go` | Three attempts per slot, two-minute retry delay, five consecutive failures before a pause; misses older than one hour require a decision. | Temporary unavailability consumes failure limits and missed work does not always catch up. |
| Rex `internal/rex/scheduler.go`, `Recover` | Searches for a run using a claim's host ID; a claim without a discovered run becomes interrupted. | Admission uncertainty and temporary lookup errors must remain retryable. |
| Rex `internal/rex/scheduler.go`, `Tick` | Advances the schedule before separately creating a claim. | A crash between these writes can lose an occurrence. |
| Runtime `internal/engine/engine.go` | Most adapter errors finalize the workspace and mark the run failed. | A temporary model transport failure loses eligibility for checkpoint recovery. |
| Runtime `internal/engine/engine.go`, `RecoverInterruptedLightweightRuns` | Startup recovery scans queued/running lightweight runs and excludes job-owned workers. | Needs an ongoing, persisted recovery path for transient waits. |
| Runtime `internal/workjobs/controller.go` | Recovers nonterminal workers, closes terminal attempts, and protects unresolved items after mutations. | Provider failures become failed attempts and may reach no-progress/item limits or uncertain-effects waits. |
| Runtime `internal/runtime/native_checkpoint.go` and `native_recovery.go` | Stores transcript, completed tools, started mutation markers, and usage. | Reuse these semantics; do not replace them with prompt replay. |
| Runtime `internal/durable/activities.go` | Reserves infrastructure retries for activity interruption; ordinary execution errors are nonretryable. | Keep this unchanged for the initial lightweight-only feature. |
| Rex `src/chatHelpers.ts`, `TurnView.tsx`, and `Jobs.tsx` | Presents failures and user-driven retry/resume controls. | Needs an automatic waiting state distinct from failure and human input. |

The visible provider-busy message is insufficient to diagnose the specific
reported incidents. Recovery decisions must use structured errors from the
failing operation, with the raw sanitized cause retained for diagnostics.

## Invariants

1. A transport retry preserves run ID, host run ID, conversation turn ID, native
   resume key, and job attempt/item ownership. It adds no synthetic human reply
   and does not reset the task's no-progress history.
2. Exactly one execution owner may dispatch a given run at a time. Persisted
   generation checks reject stale state writes and stale job submissions.
3. A wait is durable before execution ownership is released. A crash at any
   point leaves either a recoverable running run or a recoverable waiting run.
4. Completed tools and accepted item results are reused. External side effects
   are not promised exactly once; uncertain outcomes retain existing safeguards.
5. Temporary infrastructure waits do not consume logical routine attempts,
   item attempts, or no-progress attempts. Actual model usage still counts.
6. User cancellation, manual pause, approvals, authentication, and explicit
   budget limits take precedence over automatic retry.
7. Closing the window, disconnecting the event stream, or losing the Rex-to-
   runtime HTTP response does not prove execution failed.
8. A waiting run is neither successful nor terminally failed. It has no
   `completed_at`, and its workspace/checkpoint remains available.

## Recovery contract and persistence

Use the existing run status `paused` with a new pause reason
`transient_failure`. Add an optional typed `recovery` object to `AgentRun`.
This avoids adding a seventh top-level status while giving hosts an explicit
machine-readable distinction from human and manual pauses. All generic paused
branches must be audited; adding the reason alone is insufficient.

Proposed recovery fields:

| Field | Purpose |
| --- | --- |
| `reason` | `network_unavailable`, `connection_lost`, `provider_busy`, or `dependency_unavailable`. |
| `attempt_count` | Transport recovery attempts in the current failure episode; separate from task/job attempts. |
| `first_failed_at` | Beginning of the current episode. |
| `last_failed_at` | Most recent failed execution attempt. |
| `next_retry_at` | Persisted UTC eligibility time. |
| `provider_retry_at` | Optional hard lower bound derived from `Retry-After`. |
| `last_error_code` | Stable diagnostic category; sanitized detail may accompany it. |

Store the recovery payload and an indexed retry timestamp on `agent_runs`.
Update SQLite GORM records, PostgreSQL migrations, row conversions, in-memory
stores, clone helpers, SDK types, OpenAPI, and affected client types/fixtures.
Write status, pause reason, retry timestamp, and payload atomically. Add
conditional store operations for entering a wait and claiming a due retry;
match the expected state and recovery version/generation so a cancelled or
newly edited run cannot be resurrected by an older attempt.

Retain recovery metadata during retry execution for diagnostics and consecutive
backoff calculation, but expose the waiting presentation only while the pause
reason is `transient_failure`. Clear the episode after a successfully completed
model exchange or tool-result checkpoint demonstrates forward progress. Preserve
history through persisted recovery events. Do not reset backoff merely because
a socket opened or a partial response arrived.

Advertise an automatic recovery capability only when the runtime has the
required store operations, native checkpoints, execution mode, and enabled
policy. Rex must verify this capability before enabling its new waiting UX and
removing the old retry fallback for that runtime.

Automatic recovery uses an internal execution-control path. Do not route it
through Rex's human `ResumeRun` wrapper or change `last_resume`: that wrapper
labels input as human and the normal resume path can create a new turn identity.
An explicit user “Retry now” control may wake recovery through an authenticated,
app-scoped endpoint; it cannot resolve an approval or bypass a provider deadline.

## Failure classification

Normalize failures at model/provider and dependency boundaries into typed causes.
Prefer HTTP status, provider error code, wrapped `net.Error`, and stream error
types. Keep narrowly tested string matching only for legacy adapters that cannot
yet expose a structured cause.

| Failure | Decision |
| --- | --- |
| Connection reset, broken pipe, unexpected EOF, incomplete model stream | Retry from the saved model boundary. |
| Temporary name-resolution failure, unreachable network, endpoint refused | Retry; include a precise dependency reason. Invalid endpoint configuration is a separate error. |
| HTTP 408, temporary 429, 500, 502, 503, 504 | Retry, subject to provider-specific permanent error codes and `Retry-After`. |
| HTTP 429 with exhausted quota/billing code | User action; do not treat as temporary throttling. |
| Request-scoped timeout with the run still authorized | Retry when the operation and checkpoint are recoverable. |
| Explicit user cancellation or exhausted overall execution budget | Stop; never reinterpret as a transient request timeout. |
| Worker shutdown/context cancellation | Preserve restart recovery; distinguish it from user cancellation using durable run state. |
| Invalid API key, login required, permission denial | Existing authentication/action-required behavior. |
| Invalid model, invalid arguments, context limit, malformed configuration | Existing failure or repair flow; no endless automatic retries. |
| Checkpoint/storage corruption or persistence failure | Surface the storage problem; do not retry execution from an untrusted checkpoint. |
| External tool request with an uncertain mutating outcome | Existing outcome reconciliation; never generic tool replay. |

Transport failures returned inside a tool's `is_error` result need explicit
consideration: they may not reach the adapter error path. Carry a structured
recoverability marker only for known safe transport failures. Preserve the tool
result and checkpoint before pausing. On recovery, let the model re-evaluate a
read or inspect remote state; do not rerun every failed tool automatically.
Ordinary site errors and task-specific item exceptions retain their existing
model/job policies.

## Retry policy and execution ownership

Start with retry intervals of 5, 10, 20, 40, 80, 160, and then 300 seconds, with
bounded jitter. These are proposed defaults to verify in the fault tests. The
five-minute cap applies to ordinary backoff; a valid longer provider deadline
must be honored rather than clamped down. Persist the chosen timestamp so a
restart does not reset backoff or sample new jitter repeatedly.

Do not impose a small retry-count or elapsed-outage limit on classified temporary
failures. Keep waiting until recovery, cancellation, an explicit budget/deadline,
or a newly identified permanent error. Sleep/offline time does not count as active
model/tool execution time. Existing wall-clock deadlines must be identified and
either intentionally preserved as task deadlines or changed to active-work
accounting; retry must not silently erase a real user deadline.

Do not stack minutes of provider SDK retries beneath minutes of runtime retries.
Audit existing provider retry loops, including `native_chatgpt.go`; retain only
bounded request-level retries before yielding to durable recovery. Preserve
provider retry instructions across this boundary.

| Run type | Recovery owner | Other components |
| --- | --- | --- |
| Ordinary lightweight chat or routine run | Runtime recovery loop | Rex watches; the scheduler retains the occurrence. |
| Lightweight job worker | Existing work-job controller | Generic run recovery excludes it; Rex only presents state. |
| Durable/Temporal run | Existing workflow | New lightweight recovery never dispatches it. |
| Accepted host request without a linked runtime run | Rex admission reconciliation | Reuses its stable dispatch key until run linkage is known. |

Build the ordinary recovery loop on the existing single-owner lightweight
recovery setup. Scan due rows in bounded batches, recheck state and group
controls, acquire the execution guard, and restore the checkpoint. Integrate the
due-time/state guard at the final dispatch point as well: `launchLightweight`
currently queues another execution when a launch races an active execution.
A burst of wake hints must not turn that behavior into an immediate retry loop.

For jobs, retain the existing generation fencing in `recoverWorker` and add the
same due-time and state checks before bumping a generation or dispatching. A
failed claim or an already active execution must not consume another generation
or recovery attempt unnecessarily.

Keep recovery admission bounded and fair across conversations and jobs. Preserve
browser-profile serialization. During a known provider cooldown, hold new work
using that provider/credential scope rather than opening every job worker just
to receive the same 429. Do not block unrelated providers, credentials, local
models, or non-network tools. Reuse existing capacity controls and derive
cooldown eligibility from persisted waits; avoid a separate generic circuit-
breaker service for this feature.

## State transitions and user controls

| Current state and event | Transition |
| --- | --- |
| Running, recoverable failure | Persist `paused/transient_failure`, checkpoint, and next retry; release execution. |
| Recovery wait, timer due or valid recovery hint | Conditional claim, clear waiting pause, execute the same run from its checkpoint. |
| Retried execution, another temporary failure | Return to recovery wait with increased backoff. |
| Retried execution, successful progress | End the failure episode and continue normal execution. |
| Recovery wait, user cancels | Cancel terminally, invalidate retry claims, and perform existing terminal cleanup. |
| Job recovery wait, user pauses job | Preserve checkpoints and prevent dispatch until explicit job resume. |
| Routine recovery wait, user disables routine | Stop automatic starts/recovery for that routine; retain pending work for explicit re-enable or cancellation. |
| Recovery wait, new user message | Serialize with recovery; deliver the genuine message once through the existing turn semantics and invalidate obsolete retry claims. |
| Any run, approval/authentication/manual-input pause | Remain under the existing interaction flow; network recovery cannot resolve it. |

A new message in a waiting conversation must not cause Rex's “resume failed;
starting a new run” fallback to race the old run. Explicitly settle or supersede
the interrupted turn according to Rex's existing message behavior before
accepting a replacement execution. A mere “Retry now” is execution control, not
a new conversation message.

The first delivery needs cancel for waiting chats, pause/resume for jobs, and
enable/disable for routines. Do not broaden manual-pause support for all
lightweight chats as part of this fix.

Routine enablement lives in Rex while retry dispatch lives in the runtime.
Bridge this explicitly: persist a host recovery hold against the linked run
when disabling a waiting routine, and clear it on re-enable. Use an app-scoped,
idempotent control that does not alter the turn identity. Serialize disabling
with admission and reconcile any unacknowledged hold before dispatch after
startup. The runtime must enforce the hold at the final claim/dispatch check;
stopping Rex's watcher alone cannot prevent a background retry. An already
running operation may finish, but no subsequent automatic recovery may start
after the hold is acknowledged. Job pause uses its existing group controls.

## Job behavior

Extend `reconcileAttempts` to distinguish transient pauses from worker input,
authentication, and terminal failures. Keep the attempt open, retain item claims,
and preserve accepted result receipts. Resume the same attempt/run after its
retry deadline; do not close and recreate it to obtain another model attempt.

Add a job waiting reason such as `worker_recovery` to the shared SDK contract.
When every open worker is waiting for recovery and no eligible work can proceed,
show that reason. When healthy workers remain, keep the job running and show the
individual waiting workers. Mixed recovery/auth/input waits must still expose
the required user decision and all affected workers, without labelling transient
waits as requests for input.

Keep these accounting distinctions explicit:

- Recovery attempt counters increase only for actual recovery executions.
- Item-attempt and consecutive-no-progress counters do not increase during an
  infrastructure wait or same-attempt recovery.
- Existing item-exception retries remain separate from transport recovery.
- Model usage already incurred remains charged once, using checkpoint usage
  watermarks; waiting itself consumes no model tokens.
- Completed datasets, result receipts, and job notifications remain idempotent.
- A real budget limit or unresolved mutation still blocks the appropriate work.

Waiting workers release active execution resources, but keep exclusive claims on
their assigned items. Audit `schedule` and worker-count accounting so open
waiting attempts do not prevent unrelated healthy work indefinitely. Browser
ownership may remain reserved to preserve a session; distinguish that reservation
from an actively executing worker and keep same-profile operations serialized.

Rex's job watcher must continue polling recovery-wait jobs, avoid sending a
“needs your decision” notification for them, and display retained progress.
Final result delivery retries in `internal/rex/jobs.go` remain their own concern.

## Schedule and admission durability

Replace the sleep/missed grace decision with automatic latest-occurrence
catch-up for enabled routines. Preserve the original `scheduled_for` timestamp
and provide both scheduled and actual start times in routine context/history.

Make advancing `next_run_at` and inserting the occurrence claim one SQLite
transaction. The existing unique occurrence/attempt key remains the deduplication
authority. A crash cannot advance the schedule without leaving recoverable work.

For each pending claim:

1. Look up its linked runtime run or stable `host_run_id`.
2. If a lookup is unavailable, leave the claim pending; an error is not absence.
3. If the runtime confirms no run exists, submit using the same host ID.
4. If the response is lost, reconcile that ID on the next pass instead of
   declaring failure or creating another claim/turn.
5. Attach the recovered run to the existing turn and watch it to settlement.

Persist the logical turn/claim linkage before remote admission and reuse it
across retries. Apply the same rule to accepted chat turns without a runtime ID.
Persist enough of the validated admission intent to replay the exact accepted
request after a restart; do not reconstruct a different prompt under the same
idempotency key after bot settings/history change. Use existing protected
credential storage and references rather than persisting plaintext credentials.

Limit coalescing to occurrences whose admission has definitely not begun.
An uncertain admission must first be looked up using its original dispatch key;
it cannot be relabelled as a newer slot. Record superseded unstarted occurrences
as skipped so history explains why only the latest slot ran.

Routines in runtime recovery wait remain open. They are not successful paused
runs, and they do not call `afterFailure`, increment consecutive failures, or
start a fresh routine attempt. A busy conversation is an admission wait, not a
task failure. Preserve bounded task-failure handling for genuinely failed runs.

Serialize ordinary chats, routine catch-up, and job browser use according to
existing conversation/profile ownership. Do not let an active routine and its
catch-up occurrence drive the same browser simultaneously.

On upgrade, do not automatically revive all historical failed turns/jobs or
previously dismissed missed schedules. Recover pending work that has an explicit
durable recovery/admission record. For older failed runs, provide an explicit
“Resume saved progress” route only if the checkpoint and side-effect evidence
make continuation valid; otherwise retain the existing manual retry flow with a
clear explanation. That historical repair is separate from new automatic waits.

## Wake and reconnect integration

Rex uses Tauri, not Electron. Put lifecycle handling in the native host/desktop
layer so it works with the window closed or minimized. Browser `online` events
can be supplemental hints but cannot be the only recovery trigger or proof that
a provider is reachable.

On macOS wake, Windows resume, runtime readiness, and network-change signals:

1. Coalesce repeated hints and confirm the host/runtime are ready.
2. Reattach run/job watchers and reconcile pending admissions and routine slots.
3. Ask the runtime recovery owners to reconsider network-related waits.
4. Prepare the browser/profile/extension bridge only for admitted executions.
5. Retry when the relevant dependency can be reached; preserve provider-imposed
   cooldowns and user controls.

Use a small authenticated host-to-runtime recovery wake operation. It is a hint
to the owners, not a command to start every run. Repeated hints must be cheap and
idempotent. Native events accelerate retries; a host-side periodic fallback
continues working if an event is missed. Use wall-clock gap detection as a
fallback for sleep, and targeted, rate-limited reachability checks only while
network waits exist. Avoid dependence on a public “internet check” service.

A reconnect hint may bring a network-loss retry forward once per coalesced
recovery episode. It must not bypass a `Retry-After` deadline, continually reset
backoff during network flapping, or assume a live model stream has died.

Keep runtime process health separate from provider reachability. The Rex
supervisor must not repeatedly restart a healthy local runtime merely because
its model provider is offline or throttled. Likewise, a broken UI event stream
should trigger reattachment, not execution recovery.

Acceptance target: after a wake/reconnect signal and dependency readiness,
reconcile eligible work within five seconds, subject to provider deadlines and
execution capacity. Without native signals, detect recovery within thirty
seconds through the fallback. Measure these targets in packaged desktop tests;
time spent on login, user approval, or a provider's cooldown is excluded.

## Presentation and events

Keep the Rex turn open while waiting. Add recovery-aware run projection to its
API/UI so refreshing the page reconstructs waiting state from storage rather
than requiring the event that announced it.

Suggested copy:

- Network: “Waiting for connection. This bot will resume automatically.”
- Provider: “The model provider is busy. Retrying automatically.”
- Dependency: “Waiting for the browser connection. Work will resume automatically.”
- Resuming: “Connection restored. Continuing from saved progress.”
- Job: “Waiting for connection · 27 of 100 items completed.”
- Late routine: “Scheduled for 08:00 · started at 09:15 after wake.”

Keep completed steps and partial response text visible. Stop the active-work
spinner and avoid counting offline time as “Worked for …”; derive wait durations
from recovery lifecycle timestamps/events. Show the next retry when known and a
cancel control. Human-input and permanent-error states retain their own actions.

Persist explicit recovery-wait and recovery-started events, alongside compatible
paused/running lifecycle events, with reason, retry time, and execution/turn
identity. Update `runStream.ts` explicitly: its current lifecycle handling must
not infer a run status by blindly stripping the prefix from new event names.
Test reconnect/replay and repeated events for duplicate text or steps.

Update routine history, job worker rows, attention badges, notifications, and
active-run counts so automatic waits are visible but do not demand user action.
Sanitized raw errors belong in diagnostics, not the primary status text.

## Implementation sequence

### 1. Reproductions and shared contract

Create deterministic fake-provider fixtures for a dropped stream after tools,
temporary 429/503 with retry headers, prolonged offline operation, and restored
connectivity. Add job fixtures with accepted results before interruption. Use
injected clocks to avoid real multi-minute sleeps.

Add SDK recovery fields/constants, capability shape, job waiting reason, and
recovery-wake client operation. Update runtime aliases, OpenAPI schemas,
serialization fixtures, and affected React/console types. Add store migration,
round-trip, and conditional-transition tests.

Exit: old rows decode without recovery fields; new waits survive store reopen;
stale claims cannot overwrite cancellation; contract tests pass.

### 2. Runtime recovery for ordinary lightweight runs

Implement typed failure classification, wait persistence before terminal cleanup,
checkpoint continuation, the bounded recovery scan, and wake hints. Apply it to
all execution branches that can make model requests, including completion-policy
enforcement. Recheck permissions, credentials, endpoint bindings, and current
group state before recovery, as normal execution does.

Preserve the original turn/resume identity and existing native outcome markers.
Exclude durable and unsupported adapter executions through an explicit policy
gate. Add regressions proving existing Temporal behavior is unchanged.

Exit: a multi-step chat survives disconnect, an overnight wait, and runtime
restart with the same run/turn and no replayed completed tool.

### 3. Job controller integration

Teach reconciliation about transient waits, due retries, worker generations,
mixed pause reasons, capacity, and provider cooldowns. Keep attempts and items
open and avoid changing logical retry/no-progress counters. Exercise cancellation
and job pause concurrently with recovery.

Exit: a partially complete multi-worker job recovers to completion with identical
accepted results, no duplicate submissions/effects, and correct usage accounting.

### 4. Rex admission and routine integration

Persist/reconcile accepted admissions, make claim-and-advance atomic, catch up
the latest missed routine, and retain waiting occurrences. Remove the fresh-run
transient fallback only when the runtime capability is active. Update paused-run
settlement so recovery waits never settle a turn or routine as successful.

Exit: crashes before admission, after admission, and before linking all converge
on one turn, one occurrence, and one runtime run. A missed overnight routine starts
automatically once without consuming a failure attempt.

### 5. Desktop signals and presentation

Wire native wake/network hints and the background fallback. Update waiting
presentation, job/routine views, streams, badges, controls, and active-work
duration. Refresh/reopen must show the same persisted state.

Exit: recovery works with the window hidden on macOS and Windows, and UI wording
clearly distinguishes automatic waiting from required user action.

### 6. Release and verification

Release the additive SDK contract, then the runtime implementation with the
feature opt-in, then Rex pinned to both released versions. Keep local module
replacements confined to development checks; do not ship them. Rebuild sidecars
and packaged desktop apps so tests exercise the actual bundled runtime.

Enable by default for supported Rex runtimes after the complete test matrix
passes. A kill switch may stop new recovery dispatch, leaving existing waits
inspectable and cancellable. Roll back behavior using the new state-aware binary;
do not downgrade to an older runtime that misinterprets `paused/transient_failure`.
Preserve additive columns and pending records during rollback.

## File and contract checklist

| Repository | Expected implementation touchpoints |
| --- | --- |
| agent-runtime-go | `types.go`, `work_jobs.go`, client/API methods and tests, recovery/capability contracts. |
| agent-runtime | `internal/agentcore/types.go`, `work_jobs.go`; provider/native error boundaries; `internal/engine/engine.go`, `work_jobs.go`, run/group control paths; store records/migrations/in-memory implementation; `internal/workjobs/controller.go` and service; API/OpenAPI; runtime startup wiring and capability configuration. |
| agent-runtime | Existing native recovery, turn identity, usage watermark, work-job fencing, and durable workflow tests; extend rather than duplicate these suites. |
| Rex host | `internal/rex/host.go`, `runtime.go`, `store.go`, `scheduler.go`, `jobs.go`, `server.go`, browser readiness, runtime supervisor/startup wiring, and related tests. |
| Rex desktop/UI | `src-tauri/src/lib.rs` and platform lifecycle integration; `src/types.ts`, `runStream.ts`, `chatHelpers.ts`, `TurnView.tsx`, `Jobs.tsx`, routine/conversation controls, attention/notification handling, and tests. |
| Documentation | Runtime lifecycle/API documentation, SDK contract examples, Rex schedule/recovery behavior, and release notes explaining automatic catch-up. |

Before editing, enumerate every caller and status/reason switch for the affected
types. In particular, audit terminal cleanup, idle chat expiry, `ResumeRun`,
routine success-on-pause, job worker-input detection, notification logic, browser
reservations, and partial-stream rendering.

## Acceptance test matrix

| Test | Expected result |
| --- | --- |
| Offline before first request | Accepted turn stays open, then completes automatically. |
| Model stream drops after four completed tools | Same run/turn continues; each completed tool executes once. |
| Stream drops after partial assistant output | UI preserves useful partial output without duplicating settled answer text. |
| Temporary 429 with long `Retry-After` | No early request, including after reconnect or restart. |
| Quota-exhausted 429 or invalid credentials | Specific user-action state, no infinite transport retry. |
| Outage lasts eight hours | Pending work survives; attempts do not exhaust routine/item/no-progress budgets. |
| Runtime crashes after wait save | Wait is recovered from storage. |
| Runtime crashes before wait save | Existing interrupted-running recovery finds the checkpoint. |
| Crash after retry claim but before execution | Ownership is recoverable; run is not stranded as executing. |
| Simultaneous timer, wake, and reconnect | One execution owns the run; no immediate retry spin. |
| Local model while internet is unavailable | Local execution proceeds. |
| One provider throttles while another is healthy | Only affected work waits; healthy work retains capacity. |
| Job has 27 accepted items, then loses connection | Continues remaining work without duplicate accepted rows or counter inflation. |
| Job workers mix healthy, recovery-wait, and auth-wait | Healthy work proceeds and required sign-in stays visible. |
| Old worker submits after new recovery generation | Stale submission is rejected. |
| User cancels while a retry is claimed | Cancellation wins; no later tool or model dispatch from the stale claim. |
| User pauses a waiting job or disables a routine | Reconnect does not resume it; explicit resume/re-enable is required. |
| User sends a new message during recovery | Message is applied once; no competing old/new runs in the conversation. |
| Sleep spans several recurring slots | Active occurrence resumes; at most one latest unstarted occurrence follows. |
| Daily routine is more than one hour late | Runs automatically with original scheduled time recorded. |
| Schedule crosses DST or clock adjustment | Existing timezone semantics preserved; no duplicate occurrence identity. |
| Crash between schedule advancement and claim | Transaction leaves either both persisted or neither. |
| Admission succeeded but HTTP response was lost | Stable host ID finds the same run and links it to one existing turn. |
| Routine recovery lookup returns a network error | Claim stays pending; not marked absent, failed, or successful. |
| Disconnect during a mutating action | Known result reused; unknown outcome reconciled before any repeat. |
| Browser closes or page changes during outage | Browser readiness and fresh references are established before continuation. |
| Human approval/input pause plus reconnect | No approval/input is fabricated by recovery. |
| UI reload or event-stream reconnect during wait | Correct waiting state, steps, counters, and controls restored from persisted data. |
| Repeated retries and process restart | Usage watermarks prevent double accounting; actual new model calls still count. |
| Unsupported store, adapter, or durable execution | No capability advertised; existing behavior remains intact. |
| Feature disabled after waits exist | Waits remain visible/cancellable and are not silently discarded. |

Run focused Go unit/integration tests for SDK, runtime/store/engine/jobs, and Rex
host, including race-enabled lifecycle tests. Run Rex Vitest tests and
`pnpm build`; run the relevant desktop Rust checks/builds for platform hooks.
Exercise PostgreSQL migrations in the existing database integration setup even
though Rex uses SQLite. Complete packaged macOS and Windows checks with actual
sleep/wake, network loss/restoration, hidden window, and runtime restart.

Record recovery latency, run/turn/attempt IDs, model/tool call counts, accepted
item counts, usage totals, and cancellation outcomes in test evidence. Structured
logs should include recovery reason, trigger, eligibility time, and generation,
without prompts, tokens, credentials, or sensitive request headers.

## Completion criteria and limits

Delivery is complete when the reported multi-step chat scenario, an overnight
missed schedule, and a partially completed job all resume automatically after
connectivity returns, without user retry messages, lost progress, duplicated
completed work, or exhausted logical retry budgets. The same behavior must
survive Rex/runtime restarts and work with the window hidden.

The design intentionally retains user decisions for credentials, explicit
budgets, approvals, permanent failures, and unresolved side effects. It does not
automatically revive historical terminal runs, replay every missed occurrence,
add a new workflow service, or enable new Temporal recovery semantics. Those
limits do not prevent automatic recovery of newly accepted Rex work.
