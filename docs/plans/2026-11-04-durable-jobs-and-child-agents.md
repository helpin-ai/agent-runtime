# Durable jobs and parallel child agents

Status: proposed implementation plan; no runtime behavior has changed.
Date: 2026-11-04
Revision: integration-review decisions incorporated; phase 0 still requires executable contract fixtures.
Second review: exclusive recovery ownership, SQLite process locking, result submission, memory-store behavior and capability detection specified below.
Third review: runtime primitives separated from the optional job module; phase 1b gated on a committed Helpin pilot; phase 0 pilot added; phase 3 narrowed and procedural learning split into its own project.
Fourth review: run groups made independent of work jobs (own identity, capability, routes); atomic terminal-write/completion-sequence contract and host consumption contract; SDK compatibility separated from feature adoption.
Scope: agent-runtime, agent-runtime-go, and Rex. Helpin is the expected second consumer; Usermaven must remain unaffected. Existing integrations remain opt-in.

## 1. Outcome

A user assigns a bot a substantial task once, such as researching forums at 1,000 schools and producing a sourced dataset and data model. The system processes bounded units of work, verifies persisted results, automatically continues, and survives interruptions without requiring repeated “next batch” messages.

The same bot can execute several units concurrently. Workers share a versioned identity, relevant skills, authorized long-term memory, and job specification. Each worker has independent working context and execution state. The user sees one job with understandable progress, exceptions, costs, and controls.

Success means verified completion against an explicit contract, not merely a long-running model session. Drift must be constrained and detected; zero drift is not a guarantee.

### Consumers and reuse thesis

- **Rex (first consumer).** Desktop app with no backend; nothing else in Rex can own a durable queue, leases, budgets and verification. Phase 1a alone delivers the core value: assign once, recover after sleep/restart, verified coverage, one job card. Phase 2 adds throughput for fetch/search work; browser work stays serialized per profile.
- **Helpin (expected second consumer).** Helpin already runs host-side orchestration: command-bar parent plans that delegate steps to child runtime runs, advanced by a projection finalizer plus `SweepStalledCommandBarPlans` for missed terminal events, alongside scheduled-event leases and playbook launch recovery. Its gain is replacing inferred run state with runtime primitives (run groups, reliable child completion, cascading cancel, aggregate budgets) and deleting stall-recovery code, not new features. Helpin migrates one flow at a time behind a flag and keeps its sweeper as a backstop until the runtime completion signal has a production record.
- **Usermaven.** Interactive analytics chat; no expected direct use. Its requirement is that nothing changes.

Domain logic never moves into the runtime: CRM enrollment, playbook and command-bar plan semantics, approvals UI and business validation stay in the apps. The runtime provides generic primitives; flow semantics belong to the host.

## 2. Findings and existing foundations

These findings come from the local code inspection accompanying this plan. Before implementation, compare them with the current remote branches and resolved SDK version.

- `internal/runtime/native_checkpoint.go` and the native tool journal already preserve run execution state.
- `internal/runtime/native_exec.go` implements bounded model execution and parallel read-only tool calls. It does not expose a native child-agent lifecycle.
- `internal/engine/engine.go` validates turn completion and manages run transitions. A valid `finish_turn` is not proof that an entire external work queue is complete.
- `internal/durable/workflow.go` supplies Temporal orchestration, retries, continuation, and waits for input/authentication/approval. Reuse it rather than adopting another orchestration framework.
- `internal/runtime/a2a.go` delegates a turn to an external agent. This is not automatic inheritance of a local bot's identity, skills, tools, or memory.
- `internal/tools/memory_tools.go` authorizes memory through app/bank scope. Multiple authorized runs can use the same bank.
- `docs/memory.md` explicitly separates long-term memory from native checkpoints and context compaction.
- Rex's `internal/rex/bots.go` assembles identity, operating rules, and enabled skills. Memory is reference evidence, not permission or authoritative instructions.
- Rex's operational memory pipeline extracts supported outcomes and consolidates lessons. It does not generate tested, executable workflows.
- Rex's `internal/rex/scheduler.go` schedules recurring routines while the desktop is available. A recurrence schedule is different from advancing an unfinished job.
- The inspected runtime and SDK lack native spawn/join APIs and persisted parent/child ownership with aggregate budgets and cancellation.
- Helpin's `internal/service/command_bar_plan_orchestration.go` schedules pipeline/DAG steps as delegated runtime runs, advances them from run projection finalizers and relies on a periodic stall sweep because a host can miss terminal events. `automation_scheduled_event.go` and `crm_playbook_agent_launcher.go` add database-leased dispatch and launch recovery. This is evidence that hosts need run-group primitives, and that host-side orchestration compensates for state only the runtime holds.

Existing checkpoints and queues are foundations, not evidence that the full job feature exists. Do not confuse memory-learning jobs with the work-execution jobs proposed here.

Integration baseline supplied by the review: Helpin uses Go SDK v0.7.0, Rex uses v0.9.0, and Usermaven uses HTTP from Python. Unknown JSON fields/events are tolerated by the reviewed clients, but that does not make unsolicited child-run callbacks safe. Rex runs the runtime as a separate process; its direct runtime package dependencies are `memory/`, `browser/`, and `sandbox/wsl`. Internal engine/store/durable changes reach it through API/events. Phase 0 records these baselines against exact integration revisions; Helpin must not be forced to upgrade its SDK.

## 3. Separate identity, knowledge, and execution

| Layer | Responsibility | Update policy |
| --- | --- | --- |
| Core identity (“soul”) | Stable role, standards, communication, boundaries | Deliberate, versioned edits; never rewritten by memory consolidation |
| Skills/procedures | Reusable methods and validation instructions | Versioned; learned procedures require evaluation before promotion |
| Long-term memory | Facts, preferences, supported lessons and limitations | Evidence-backed extraction/consolidation with provenance |
| Job specification | Objective, input set, deliverables, scope, completion criteria and budgets | Versioned; user corrections explicitly revise affected work |
| Job state | Items, dependencies, ownership, attempts, accepted results and blockers | Authoritative transactional records |
| Working context | Current assignment, relevant evidence and recent tool results | Bounded; compact or replace between sessions |

The job must remain recoverable if the conversation is discarded or memory retrieval is unavailable. Memory is not the queue, a completion ledger, or a channel for worker coordination.

Pin identity, skill contents/revisions, extraction schema and relevant execution configuration for the job. Record the effective model for each attempt. Credential rotation and permission revocation remain live and must not be frozen into the snapshot. Do not store secrets in snapshots.

Share the authorized memory bank, not the entire parent transcript. Recall relevant information per assignment and record which evidence informed an attempt. New memories may become available between attempts; they cannot silently change the job's criteria or pinned procedure.

## 4. Architecture and ownership

```text
Rex: bot setup, job specification, progress, user corrections and approvals
                         |
              agent-runtime-go typed APIs
                         |
agent-runtime: durable job controller + transactional work-item store
                         |
             bounded child-run dispatcher
                  /       |       \
               run A    run B    run C
                  \       |       /
          evidence validation + accepted results
                         |
               final deliverable verification
```

| Repository | Owns |
| --- | --- |
| agent-runtime | Run-group primitives (below) plus the optional job module: job lifecycle, claims/leases, dispatch, recovery, budgets, dependency readiness, validation hooks, progress events and authorization |
| agent-runtime-go | Additive typed contracts, transport methods, event/callback contracts, capability negotiation and thin helpers (for example wait-for-children). No orchestration logic or persistence |
| Rex | Bot identity editing, user-facing job setup, dataset-specific criteria, notifications, approvals, job/worker UI and local resource configuration |
| Helpin (when adopting) | Flow semantics (command-bar plans, playbooks, enrollment), CRM approvals/audit and business validation, built on runtime run groups and completion signals |

### Layering: primitives, job module, app policy

**Runtime primitives** are the minimum that requires authority over the run lifecycle; apps cannot implement them correctly from outside. They have no dependency on work jobs: a host can use run groups without creating a job, and nothing in this list references items or attempts.

- **Run groups.** An app-scoped `agent_run_groups` record (group ID, idempotent creation key, owner kind `host` or `work_job`, optional parent run, admission/budget policy, pause/cancel state, next completion sequence). Runs join a group through a row in an additive `agent_run_group_members` table (app, run, group, optional parent run, execution generation, completion sequence, visibility), set at creation through an optional field on the existing start-run request. `agent_runs` is not altered (phase 0 decision: `SQL.UpdateRun` saves the full run record, so new run columns would break the unchanged Postgres schema). A run belongs to at most one group. Authorization is app/actor-scoped on the group; a supplied group ID is not authority by itself.
- **Reliable completion.** Every terminal transition of a group member is persisted atomically with its per-group completion sequence (contract below). Hosts pull completions by cursor; push callbacks are wake-up hints only.
- **Cascading pause/cancel** across a group, including members not yet started.
- **Aggregate token budgets and admission control** per group and app, including fairness between bulk and interactive work.
- **Child capability intersection:** members cannot cross app/bank, escalate permissions or bypass approvals.
- **Execution fencing:** each run carries an execution generation that increments whenever the runtime grants execution authority (dispatch, recovery, resume). Writes made on behalf of a run can be conditioned on the current generation. The job module's `submit_item_result` uses this; it is not itself a primitive.
- Later: model-directed spawn/wait that releases capacity while the parent waits.

Run groups have their own capability (`run_groups`) and routes (`/v1/run-groups`: create, get, list members, pause/resume/cancel, `GET /v1/run-groups/{group_id}/completions?after=<seq>`). They are advertised and usable independently of `work_jobs`.

**Visibility and callbacks depend on who started the run.** Members of host-owned groups were started by the host through the existing start-run API: they keep normal listing visibility, run SSE and `event_callbacks`, so adopting groups changes nothing the host already consumes. Runs the runtime creates on its own (work-job workers, later model-spawned children) are hidden from default listings and have callbacks suppressed (section 10).

#### Atomic completion contract

Today a terminal transition commits `UpdateRun`, then cleans up credentials/workspace, then emits `run.completed` (`internal/engine/engine.go:1745`); `failRun`, cancellation and other terminal paths follow the same pattern. A crash between commit and emit loses the notification. For group members:

- Route every terminal transition (completed, failed, cancelled, and blocked-terminal outcomes) through one store operation that, in a single transaction, writes the terminal status and assigns the member's completion sequence by incrementing the group's counter under the group row lock (`UPDATE agent_run_groups SET next_seq = next_seq + 1 ... RETURNING`). Do not use a Postgres `SEQUENCE` or auto-increment: they are not commit-ordered, so a cursor reader could skip a lower number that commits later. SQLite's single writer gives the same ordering.
- Credential/workspace cleanup and event emission happen after commit and must be idempotent; recovery re-runs them for terminal members whose cleanup did not finish.
- The completions endpoint returns members with sequence greater than the cursor, in order, with terminal status and outcome summary. Delivery is at-least-once and ordered per group; a lost callback costs latency, never correctness.
- Ungrouped legacy runs keep their current path unchanged.

**Host consumption contract.** A host that advances work from completions must, in one host transaction, record the consumed cursor and durably advance (or durably schedule advancement of) its own state, idempotent per (group, sequence). Any backstop sweeper must take the same lock or idempotency key, so a sweep racing a completion cannot advance a step twice. The runtime plan requires this of the Helpin pilot; implementing it is Helpin's work.

**The job module** (queue, batches, validation, automatic continuation, dataset publisher, `submit_item_result`) is optional and built only on those primitives. A work job owns exactly one run group (`owner kind = work_job`); job workers are members of it. Rex uses the module because it has no backend. Helpin may adopt it per feature or use run groups directly with its own scheduler.

**App policy** stays in the host: what the items are, enrollment, approvals, business validation and flow semantics.

**Why not orchestration in the SDK:** Usermaven is Python, so a library would be written twice and drift; a library runs in the host and needs host persistence, which recreates Helpin's existing orchestration code in every app; and cross-replica budgets, fencing, cascading cancel and child capability limits cannot be enforced from outside the runtime.

Keep school-specific extraction rules in a skill/job template and authorized validation configuration, not in the generic runtime. Phase 1a implements deterministic schema/count/persistence checks inside the runtime. Optional host validation is a later extension using an app-configured URL and token, following `memory_learning_callback`; the model cannot select its endpoint or credentials.

The desktop executor is the existing **lightweight engine**, which starts a goroutine per run and uses `RecoverInterruptedLightweightRuns` after restart. Its recovery assumes a single runtime process owns the store; the job dispatcher/reconciler inherits and enforces that ownership requirement. It is not a multi-replica executor. `internal/durable/local_workflow.go` is Temporal session handling for ephemeral coding workers, not the desktop path.

### Exclusive desktop recovery ownership and startup

The **job reconciler alone recovers job children**. Change `RecoverInterruptedLightweightRuns` to skip any run whose group is owned by a work job, regardless of whether jobs are currently enabled. Members of host-owned groups are ordinary host-started runs and stay under generic recovery. Jobs disabled after an upgrade stay unavailable/paused; generic recovery must not execute their children anyway. Do not filter these children out of internal store status scans: the reconciler still needs them.

Startup order is fixed: acquire main-store process ownership → migrate/open the supported store → recover ordinary lightweight runs → reconcile persisted job attempts/children → enable new job dispatch. Generic recovery is invoked first but does not wait for its ordinary runs to finish. Ordering alone is not a duplicate-execution guard: the explicit job-child exclusion and the reconciler's per-run execution admission guard prevent overlapping recovery ticks, resumes and new dispatch from starting the same child twice. Recovery attaches to the existing child run/checkpoint before considering a new attempt.

`AGENT_RUNTIME_RECOVER_INTERRUPTED_RUNS` is already enabled in Rex. Test startup with that flag enabled and both ordinary runs and job children queued/running: each run must execute through exactly one recovery owner.

### Main SQLite process lock — new phase 1a requirement

The main store has no process-ownership lock today. Add one using `internal/filelock`, tied to the canonical main SQLite database path (an adjacent `<database>.runtime-owner.lock` file, not a memoryjobs lock or SQLite's own byte-range locks). Normalize supported file DSNs and aliases to the same identity. Acquire it before migrations, generic recovery, job recovery or execution admission for a SQLite lightweight runtime with recovery/jobs enabled; hold it for the process execution lifetime. In-memory SQLite and non-file storage cannot provide this durable ownership contract and do not support jobs.

On lock contention, enter an observable `waiting_for_store_owner` startup state and retry with cancellable bounded backoff (250 ms increasing to at most 2 s), rather than exiting or reporting ready. Waiting continues until ownership is obtained or startup is cancelled; log rate-limited diagnostics. Rex must recognize this state and wait without repeatedly killing/restarting the new runtime. Do not unlink the lock, steal it or kill the old owner automatically. Surface persistent contention to the user.

On shutdown, stop admission, cancel/drain managed executions and stop store use before releasing ownership. Process exit releases the OS lock after a crash; the lock file's existence is not ownership. Test on macOS and Windows, including Rex's old-process/new-process overlap. Older binaries do not honor the new lock: the Rex supervisor must confirm an older runtime has exited before starting recovery with the new binary; do not claim the new lock fences old software.

Use Temporal for deployed execution where it is already configured, introduced in phase 1b. Both drivers call the same database state transitions. The database is the only authority for jobs, items, leases, budgets and accepted results. A Temporal job workflow only wakes up, invokes bounded dispatch/reconciliation activities and waits; it does not carry the item list, statuses, results or counters in workflow history. Activities return compact wake/status information and consult the store on every dispatch. Existing child-run workflows retain their execution history, but cannot decide job completion. Continue-as-new may bound driver wake-up history; it carries only identifiers/configuration, never a second copy of job state.

Local persistence allows recovery after sleep/restart; it does not run work while the computer is off. Uninterrupted execution requires an always-on worker. Moving a job to another worker requires compatible artifact storage, credentials and capabilities; local browser sessions are not implicitly portable.

## 5. Minimal durable records

Reuse existing run, artifact, interaction, usage and event records. Add job-specific records only for state not already represented there. New work-job routes use `/v1/work-jobs`; existing `/v1/memory/jobs` remains exclusively the memory-learning API. Internal record names may be refined during phase 0.

### Main-store ownership and database claims

Job, item, attempt, acceptance and reservation records live in the **main runtime store**, alongside `agent_runs`. Do not create a separate database/file like memoryjobs or copy its process-local mutex/file-lock claim strategy for deployed replicas.

- **SQLite (phase 1a):** use the GORM AutoMigrate path in `internal/store/gorm.go`, including explicit compatibility migrations where needed. Acquire a write transaction (`BEGIN IMMEDIATE` on a pinned connection), check job eligibility/budget, select a bounded eligible batch, and conditionally update ownership/generation while recording the attempt and reservation. Commit before any model/network work. Bound lock-contention retries. A mutex may reduce contention but is not the durable ownership mechanism; require one runtime-process owner before recovery starts.
- **Postgres (phase 1b):** add matching hand-written DDL in `internal/store/migrations.go`. In a short transaction, lock the job row for admission/budget serialization, then select eligible items in stable order with `LIMIT N FOR UPDATE SKIP LOCKED`; update leases/generations and persist the attempt/reservation atomically. Dispatch after commit. Use the same lock order for competing transitions and database time for lease expiry. Conditional generation/revision updates fence result acceptance.
- Both paths require app-scoped uniqueness, claim/readiness indexes, stable dispatch keys and rollback tests. Phase 1b adds schema-parity tests for logical columns, nullability/defaults, uniqueness, indexes and equivalent operations, allowing intentional JSON/time type differences. Every new job table has both migration paths before deployed jobs are enabled.
- Phase 1a deliberately has no Postgres migration or Temporal changes. Keep SQLite job migrations and linkage reads/writes gated by backend and capability. Do not add unmigrated columns to shared legacy run INSERT/UPDATE projections: the unchanged Postgres schema must remain usable by the new binary with jobs disabled. Test that explicitly before shipping 1a.
- **Memory store:** `driver=memory` is the unconfigured default for tests/dev. Work jobs are unavailable on it, including when an app requests the capability; reject before dispatch and never simulate durability. Implement the same public ListRuns/SearchRuns child exclusion and explicit inclusion behavior in `internal/store/memory.go` for contract parity and seeded child fixtures. Internal ListRunsByStatus remains inclusive. Use file-backed temporary SQLite for job/recovery tests, not the memory store.

### Job

- App and job ID, idempotent creation key, owner/authorization context.
- Objective, input manifest reference, expected coverage and output requirements.
- Immutable specification revision and identity/skill/schema snapshot references.
- Completion policy, validation policy, concurrency and retry limits.
- Total token/cost policy, deadline, usage and outstanding reservations.
- State, pause/block reason, next eligible execution time and timestamps.
- Final artifact references, coverage counts and accepted exceptions.

### Work item

- Stable ID unique within the job; source/input reference and optional dependency IDs.
- Specification/schema revision and required output contract.
- State, attempt count, retry time and failure classification.
- Current attempt/child run, lease owner/expiry and fencing generation.
- Evidence, accepted output and validation result references.

### Child attempt and batching

- Links to job, a bounded set of assigned item IDs and one existing runtime run; optional parent run for model-directed delegation.
- Effective agent/configuration revisions, capability scope and resource ownership.
- Usage, start/end times and structured outcome.

A worker is an execution of an existing bot definition, not a new saved bot. The durable job survives even when the conversational run that created it has finished.

One child run processes **N items per attempt**, not one run per item by default. Start with `batch_size=10`, configurable from 1–50 in the job specification; enforce an input-byte/context budget and use a smaller batch when necessary. A clean 1,000-item fixture therefore needs 100 child runs at the default, rather than 1,000 sets of run/message/event/MCP/credential rows. Measure actual row and callback counts in the pilot.

Accept results per item transactionally during the attempt; do not wait for the whole batch to finish. A crash or rejected result retries only unresolved items. Attempt membership is immutable, and a later retry creates a new assignment of unresolved items after reconciling any uncertain effects. A partial batch cannot erase accepted progress.

Linkage is the run-group membership table from section 4 (`agent_run_group_members`: `group_id`, `parent_run_id`, `execution_generation`, `completion_seq`, visibility), keyed by app and run, with app-scoped indexes. `agent_runs` and its record mapping are unchanged. There is no `job_id` column; the job record references its group, and job workers (including controller-dispatched workers without a conversational parent) are members of it. Legacy runs have no membership row. The terminal completion sequence is assigned inside the store's run update: when a member's status moves from non-terminal to terminal for the first time, the same transaction increments the group counter and writes the member's sequence, so every terminal path that calls `UpdateRun` (cancel, both completion paths, `failRun`, the Temporal failure activity) is covered without per-call-site changes. Group tables exist only where migrated (SQLite in phase 1a); stores without them never query them.

## 6. Lifecycle and completion rules

Job states: `draft`, `queued`, `running`, `waiting`, `paused`, `verifying`, `completed`, `failed`, `cancelled`. Waiting records a concrete reason such as retry backoff, authentication, input, approval, budget, or unavailable resources. Work may continue on other eligible items when a blocker is item-specific.

Item states: `pending`, `running`, `validating`, `retry_wait`, `blocked`, `completed`, `failed`, `cancelled`. A completed item's result distinguishes successful extraction, supported no-result, and an exception accepted by the completion policy. An unreachable website must not become “no forum exists.”

1. Claim eligible work transactionally and reserve its budget/resources.
2. Create or recover a child run using a stable dispatch key. Recover the crash window between recording a claim and starting the run without creating duplicates.
3. Supply the pinned job contract, assigned items, relevant skills/memory and artifact references.
4. Execute within a bounded attempt. End with a structured result, yield/checkpoint, or explicit blocker.
5. Validate outputs and evidence. Accept the result only for the current item revision and fencing generation.
6. Commit accepted progress and dispatch the next eligible work automatically.
7. Enter final verification when coverage is accounted for and no work remains active or retryable.
8. Complete only when the final deliverables and accepted-exception policy pass. Otherwise create bounded repair work or expose the unresolved failure.

A worker's final message is not a job transition. Reaching a context/tool/time limit is not success. Rotate to a new bounded attempt only when persisted progress supports continuation; otherwise consume a limited no-progress/retry allowance.

### Mid-run result submission and child completion

Inject a runtime-reserved `submit_item_result` tool only into authorized job child runs. Its input identifies an assigned item and supplies a bounded structured result/evidence or authorized immutable artifact reference. The trusted call context supplies app, job, run, attempt, specification revision and fencing generation; the model cannot choose or override them. Validate artifact ownership and bounds before reading it. Do not expose this tool to legacy runs or allow an MCP tool to shadow its name.

The tool invokes deterministic runtime validation, then checks assignment, active attempt/lease, item revision and generation in the acceptance transaction. It writes the accepted result and item state atomically and returns an explicit receipt with item ID, result revision and accepted/already-accepted status. An identical replay from the same accepted attempt/result digest returns its receipt without another write. Different replacement content, unassigned items and stale generations are rejected; they cannot overwrite accepted results. No success response may precede commit. Failed validation returns actionable field errors; unaccepted work remains unresolved. The model may continue to the next item immediately after a receipt.

Use a child-specific completion prompt in `internal/runtime/turn_completion.go` for job-linked runs only:

> Your current request is the assigned batch, under the pinned job specification. Submit each completed item through submit_item_result and require its acceptance receipt. Continue with unresolved assigned items while able. Call finish_turn(completed) only after every assigned item has an accepted outcome. Describe a concrete blocker when progress cannot continue. Your summary ends this batch attempt; it never completes the parent job. Do not ask the user to start the next batch.

Enforce that prompt in code. If a child calls `finish_turn(completed)` with unresolved items, end and classify the attempt as partial/incomplete, retain all accepted items, and route unresolved items into bounded retry or a genuine blocker state after effect reconciliation. Do not mark those items or the job successful, and do not retry forever: this consumes the no-progress/retry allowance. With all items accepted, batch completion is successful but the job controller still owns final coverage/deliverable verification. Existing ordinary-run `finish_turn` semantics and published outcomes remain unchanged. Child pause/auth/approval paths preserve their assignment; exhaustion/cancellation does not invent accepted results.

## 7. Parallelism and native sub-agents

Start with one worker, then enable a configurable bounded pool. A suggested pilot is four independent research workers, subject to provider, host and browser constraints. The controller can dispatch known batch work directly; it need not ask the model to spawn every worker.

Shared: identity snapshot, skill revisions, job contract, authorized memory bank and total budget.
Separate: item ownership, transcript/checkpoint, attempt budget, output staging and mutable browser/workspace resources.

Requirements:

- Enforce job, app, provider and scarce-resource limits; prevent one bulk job starving interactive work.
- Children inherit an intersection of authorized parent/job capabilities and their assignment. They cannot select a foreign app/bank, increase permissions, or bypass approvals.
- Delegate MCP/model access through existing runtime-managed credential paths, never through copied secrets in prompts or files.
- Reuse existing context/workspace/credential callback contracts with explicit job/child provenance for opted-in apps. Event delivery is separately restricted as specified in section 10; this does not authorize child text-delta callbacks.
- Reserve budget before dispatch, reconcile actual usage afterwards, and count retries, validation and child work toward the aggregate budget. If dollar pricing is unavailable, expose token limits and mark cost estimates rather than claiming a hard monetary cap.
- The **persisted native checkpoint's cumulative usage** is the single authority for child accounting. Associate one native child run with one batch attempt; read its committed counters and transactionally advance a durable per-run watermark while applying only the delta to the job. Native and engine `usage.checkpoint` events are wake-up hints, never separate accounting inputs; ignore their payloads for balance updates. Recovery reads the same checkpoint even if no event arrived. Phase 0 tests retries, duplicate/out-of-order notifications and final checkpoints. Counters cover reported input/output/cache/reasoning tokens, not unreported charges or pricing; retain an uncertainty/reservation bound when final usage is missing. Initial work-job children are native runs; other adapters need an explicit persisted accounting contract before being enabled.
- Pause stops new dispatch and checkpoints/intercepts active work at supported boundaries. Cancellation reaches all active descendants and releases resources without deleting accepted results. Show pending cancellation until acknowledged.
- No recursive spawning in the first version. Bounded model-directed delegation later uses the same dispatcher and persistence, not a parallel ad-hoc implementation.
- Expose typed spawn/status/wait/cancel operations when model-directed delegation is introduced. Waiting must release execution capacity; a parent cannot occupy the only available slot while waiting for its children.

Never launch several Chrome processes against the same profile. Initially serialize Extension-mode work by profile. Parallelize fetch/search work independently; allow browser concurrency only when isolated sessions and authorized login handling are supported. Use separate output staging paths. The **runtime job result publisher** owns acceptance and canonical dataset assembly: workers submit per-item outputs, and the publisher writes a versioned artifact from accepted records and atomically advances its published reference. A host validator may accept/reject evidence but does not concurrently write the canonical dataset. External CRM/export writes remain explicit authorized effects. Reuse workspace locks; use isolated checkouts when concurrent coding requires them.

## 8. Recovery, writes and verification

Expired leases allow recovery, but do not prove the old worker has stopped. Fence result commits and effect dispatch against stale attempt generations. Reconcile an existing child run before granting another worker authority over the same item.

Use existing tool journaling and operation keys where supported. An external API may complete a write before its response is lost. Do not promise exactly-once external effects: use provider idempotency keys, read-after-write reconciliation, or an explicit uncertain-effect blocker. Do not blindly repeat messages, invitations or payments.

Validation has three levels:

1. Deterministic: schema, required fields, unique IDs, persisted counts, artifact checksums and source references.
2. Evidence quality: grounded excerpts, dates, consistency and sampled source rechecks. A URL alone does not prove the extracted claim.
3. Final acceptance: coverage accounting, accepted exceptions, aggregate consistency and deliverable requirements.

An optional independent model evaluator can assist with judgment, but is not the sole authority for persistence or completeness. Repeated failures and unchanged outputs must trigger bounded escalation, not endless continuation.

## 9. User steering, memory and learned procedures

New user messages attach to the job independently of worker turns. Persist each correction once, revise the job specification, identify affected completed/in-flight items and apply the revision at a safe boundary. Reject stale results that no longer satisfy the revision. Revalidate or requeue affected results explicitly.

Capture useful outcomes from accepted/rejected attempts with job, item, attempt and evidence provenance. Sibling workers and retries are not automatically independent corroboration; consolidation must account for shared sources and correlated attempts.

Learning pipeline: recorded evidence → extracted lessons → consolidated memory → candidate procedure → fixture/pilot evaluation → deliberate versioned skill promotion. This is separate from conversation compaction. Existing jobs keep their pinned skills until explicitly migrated.

Do not block the job's next batch on optional memory consolidation. Preserve pending extraction work for later processing. A procedure update must not silently change permissions, identity, schema, or completion criteria.

## 10. SDK, API and Rex experience

Add an opt-in jobs capability and typed operations to create/get/list jobs, supply item manifests, inspect progress/results/children, revise specifications, pause/resume/cancel, and resolve blockers through existing interactions. Support pagination, idempotent mutations and optimistic revision checks.

Use the authenticated capabilities endpoint as the primary support signal, reflecting effective app opt-in, backend support and rollout availability. Absence of the new capability means unavailable/unknown for an old server; an explicit unavailable response includes a reason such as app-disabled or unsupported-store. The SDK treats 404 from `/v1/work-jobs` only as a fallback unsupported/not-available response; it must not infer from 404 whether the runtime is old or the app lacks authorization. Recheck capability after configuration/version changes. Existing capabilities fields and memory-learning routes are unchanged.

Events carry app/job/item/run/attempt identity and sequence information. Reconnection uses persisted events plus an authoritative status snapshot; the UI must not derive completion from a final assistant message.

### Run visibility and event isolation — fixed contract

- Default public `ListRuns`/`SearchRuns` results and pagination counts exclude runtime-created runs: members of work-job-owned groups and model-spawned children. Members of host-owned groups, which the host started itself, stay visible. Existing unlinked runs are unchanged. Explicit group-member listings or an authorized `include_children` filter expose hidden runs to capable clients. Internal recovery/status scans still include all runs; do not implement hiding by filtering the engine's recovery queries.
- Job progress has a separate persisted job event stream at `/v1/work-jobs/{job_id}/events` and history endpoint. Do not inject job events or child deltas into a parent run's SSE stream or Usermaven's `RunStreamState`. Explicit authorized subscription to a child run retains that run's existing event contract.
- Persist normal child events/usage for auditing and recovery, but suppress `event_callbacks` delivery for runtime-created runs by default; host-owned group members keep their existing callbacks. An app must explicitly opt into detailed child delivery. Job-enabled apps may configure a separate job-summary callback destination/types; emit coalesced progress and reliable terminal/blocker summaries there. Do not send these to existing run callbacks automatically.
- Child accounting consumes persisted usage internally and does not depend on callbacks. Callback isolation must be tested even when legacy event subscriptions use broad event filters.
- Job controls/auth/input use the job UI and existing interaction mechanisms with child provenance. Never fabricate parent text deltas or parent completion to make older clients display a job.

Rex presents one job card with objective, verified coverage, active workers, retry/block counts, budget, last progress and next action. Expand workers for detail. Batch completion is a progress update, not a request to type “continue.” Ask the user only for a real decision, missing input, authorization, or a budget/scope change.

Preserve normal chat and recurring routines. A routine may create or find an idempotently keyed job, but the job controller owns continuation. A user explicitly requesting only a small batch must not be enrolled into a larger autonomous job.

## 11. Implementation sequence and gates

### Phase 0 — Confirm contracts and baseline

Results: `2026-11-04-phase0-contracts.md`.

- [x] Inspect current runtime, SDK, Rex, Helpin and Usermaven integration contracts and local/Temporal execution paths. (Contracts §2, including the derived invariants for additive changes.)
- [x] Record a reproducible batch-stops-early fixture and current behavior. (`TestBatchStopsEarlyBaseline`; see phase 0 contracts §5.)
- [x] Turn the fixed decisions in sections 4–10 into contract fixtures (contracts §7: passing now where the code exists; the job-module fixtures are specified and land with phase 1a): main-store ownership, database-authoritative Temporal driver, batched item acceptance, child visibility, callback isolation, runtime validation/publisher ownership and usage watermarks.
- [x] Record exact integration revisions for Helpin SDK v0.7.0, Rex SDK v0.9.0 and Usermaven's Python SDK (`agent-runtime-python` v0.4.0). Preserve representative request/response/event fixtures. (Contracts §1–2; `internal/api/testdata/host_contracts/`.)
- [x] Verify cumulative usage reconciliation with duplicate events, resume/retry, missing final usage and checkpoint recovery before relying on it for admission. (No change needed; contracts §4.)
- [x] Specify/test exclusive child recovery, main-SQLite-lock waiting/shutdown, `submit_item_result` receipts/fencing, child completion, memory-store unavailability and capability-first SDK behavior before signing off phase 0.
- [x] Specify run groups independently of work jobs (record, capability, routes, visibility by owner kind) and the atomic completion contract: inventory every terminal transition path in `internal/engine/engine.go`, define the single terminal-write operation and the per-group sequence assignment, and write the host consumption contract.
- [ ] Define the school-research input/output contract and exception policy. (Draft in contracts §8; sign-off with the pilot.)
- [ ] Run a 50–100 item manual pilot before building job schema: fixed item list and output schema, one run per batch, and a throwaway Rex-side loop that re-prompts until every item is accounted for. Measure output quality, exception rates, submission ergonomics and cost per item. Discard the loop afterwards; it is not a product path.

Gate: pilot results support the item contract and exception policy; executable compatibility/usage fixtures and the fixed decisions above signed off; no requirement for a DAG editor or new framework. SQL/API spelling may be refined, but these ownership and isolation decisions are not deferred to implementation.

### Phase 1a — Rex-only SQLite/lightweight vertical slice

- [ ] Publish additive SDK contracts first; implement SQLite job tables/linkage in the main store behind a backend check and an app capability explicitly enabled only for Rex.
- [ ] Implement the run-group primitives (section 4) as a layer the job module calls, not as job-module internals: group record and linkage, `run_groups` capability and routes, atomic terminal write with per-group completion sequence, completions cursor endpoint, execution generation, cascading pause/cancel, aggregate admission and capability intersection. Test a host-owned group with no work job.
- [ ] Add main SQLite ownership locking before migrations/recovery, cancellable waiting on contention and the Rex startup-state handling. Verify old-process/new-process overlap and complete shutdown before lock release.
- [ ] Make generic lightweight recovery skip job-linked runs; run it before job reconciliation. Add a single per-child execution admission guard shared by reconciliation, resume and dispatch. Test with Rex's `AGENT_RUNTIME_RECOVER_INTERRUPTED_RUNS` enabled. No Temporal changes.
- [ ] Implement batches of ten, the trusted `submit_item_result` tool/receipt, child-specific completion contract, pinned inputs, deterministic runtime validators and runtime-owned dataset publishing. No host validator in this phase.
- [ ] Add automatic continuation, token reservations/checkpoint reconciliation, bounded retries and explicit blockers. Do not claim a hard monetary cap without pricing and complete usage.
- [ ] Add `/v1/work-jobs`, capability-first SDK detection, default child-listing exclusion and separate job events; suppress child callbacks before dispatch is enabled. Memory-store jobs remain unavailable; its listing behavior matches SQL stores.
- [ ] First test via SDK/headless fixture; then add a minimal Rex job card with creation, progress and pause/resume/cancel. Detailed worker UI and adaptive planning follow later.
- [ ] Keep Postgres migrations and legacy run-write projections unchanged. Reject job requests on Postgres and for apps without capability before any job SQL is executed.

Gate: a Rex-only 1,000-item fixture completes in bounded batches without another user prompt and survives restart. Old-SDK/HTTP fixtures pass against jobs-disabled apps, including a new binary with the pre-jobs Postgres schema. No new jobs surface, child callbacks or Temporal behavior is exposed to Helpin/Usermaven. This fixes “next batch” without committing the deployed system to unfinished job infrastructure.

### Phase 1b — Postgres and Temporal deployment support (demand-gated)

Start condition: Helpin commits to migrating one concrete flow. Command-bar plans are the intended pilot because they already delegate steps to child runs and depend on a stall sweeper. Without a committed deployed consumer, skip from phase 1a to phase 2 for Rex.

- [ ] Map the pilot flow onto host-owned run groups and the completions cursor, without work jobs. Helpin keeps plan semantics and its sweeper as a backstop behind a flag, and implements the host consumption contract (cursor + advancement in one transaction, idempotent per group/sequence, sweeper shares the same lock).
- [ ] Add hand-written Postgres DDL for every job table, nullable run linkage and indexes; verify parity with SQLite AutoMigrate and upgrade existing populated databases.
- [ ] Implement `FOR UPDATE SKIP LOCKED` claims, job-budget admission locking and generation-checked acceptance; exercise competing replica connections, not only goroutines on one connection.
- [ ] Add a thin Temporal dispatch/reconciliation driver using main-store records as the sole authority. Do not serialize item state/results into workflow history.
- [ ] Prove recovery across worker replacement, driver replay and lost wake-ups; database scans must rediscover eligible work.
- [ ] Preserve default child visibility/callback isolation and add only explicitly configured host validation using the app-configured URL/token pattern.
- [ ] Ship with the section 12 backward-compatibility tests. Deployment capabilities remain off unless explicitly enabled per app. Helpin's existing integration stays compatible with SDK v0.7.0; the pilot flow adopts run groups through a newer SDK or an explicitly chosen direct HTTP integration.

Gate: SQLite/Postgres behavioral parity, disjoint multi-replica claims, aggregate budget integrity, bounded Temporal driver history and unchanged legacy integrations. No runtime job state depends on retaining an item list inside a Temporal workflow. The Helpin pilot flow runs on runtime completion signals in staging with its sweeper recording zero required interventions over an agreed soak period.

### Phase 2 — Bounded parallel child runs

- [ ] Add bounded worker pools, resource leases, aggregate usage and cancellation propagation.
- [ ] Isolate mutable worker resources and serialize shared Extension profiles.
- [ ] Add child progress/result inspection and SDK contracts.
- [ ] Verify fair scheduling, stale-worker fencing, duplicate dispatch recovery and uncertain-write handling.

Gate: concurrent processing produces the same validated coverage as sequential processing, with no duplicate item acceptance, uncontrolled writes or budget reset.

### Phase 3 — Dependencies, adaptation and model-directed delegation

Scope dependencies to what a committed consumer needs (Rex staged jobs; Helpin's pipeline/DAG plan shapes if migrated), not a general DAG engine. Model-directed delegation is justified only where work is discovered at runtime; known item lists stay controller-dispatched.

- [ ] Add optional item dependencies with cycle detection and failed-prerequisite behavior, limited to the shapes recorded from consumer flows.
- [ ] Support staged pilot → extraction → aggregation → verification and bounded repair tasks.
- [ ] Add model-facing child spawn/status/wait/cancel operations through the existing dispatcher.
- [ ] Support versioned user corrections, controlled replanning and schema migration/revalidation.

Gate: a parent can delegate, release capacity while waiting, recover and join validated results without changing scope or permissions. Start with one delegation level.

### Separate project — Evaluated procedural learning

Not a phase of this plan; it needs its own plan and does not block phases 0–3. This plan only guarantees the hooks it will need: job/item/attempt provenance on learning evidence (section 9) and pinned skills per job. Requirements carried forward:

- Distinguish retries and sibling attempts from independent support.
- Candidate-procedure evaluation on held-out fixtures, versioned promotion and rollback.
- No automatic identity rewrite or untested procedure promotion; failed candidates cannot change active jobs.

## 12. Verification matrix

| Scenario | Required result |
| --- | --- |
| 1,000 deterministic research fixtures | Every input accounted for; persisted results and final artifacts validate; no “next batch” prompt |
| Default batch size and partial batch failure | Clean fixture uses 100 attempts for 1,000 items; item acceptance is incremental; retry only unresolved items; measure row/event/callback volume |
| SQLite ownership and transaction contention | Second runtime process cannot execute while waiting for ownership; claims/attempts/reservations commit together; database-contention retries are bounded |
| Rex restart while old runtime holds main-store lock | New process waits without starting recovery or reporting ready; proceeds once the owner exits; waiting is cancellable and no lock is stolen |
| Canonical SQLite path and shutdown ownership | Supported DSN/path aliases contend on one lock; release occurs only after execution/store use stops; crash releases OS ownership |
| Generic recovery plus job reconciler with Rex recovery env enabled | Ordinary runs and children each execute once through their assigned owner; repeated reconcile/resume cannot start duplicate child execution |
| Job capability disabled with persisted children | Generic recovery still skips children; job state remains inspectably unavailable/paused and no child executes |
| submit_item_result during a batch, crash immediately after commit | Accepted item/receipt survives and identical replay is idempotent; remaining items can continue without replaying accepted work |
| Stale/unassigned result or conflicting resubmission | Trusted generation/revision/assignment checks reject writes and preserve the accepted result |
| Child finish_turn(completed) with unresolved items | Attempt is partial/incomplete; accepted items retained; unresolved work retried/blocked within limits; job never counted successful |
| Default driver=memory and SQLite in-memory DSN | Work jobs advertised unavailable and rejected before dispatch; memory ListRuns/SearchRuns filters match SQL and internal recovery scans remain inclusive |
| Old server versus disabled app endpoint 404 | SDK consults capabilities first; fallback 404 does not claim to distinguish server age from app access; /v1/memory/jobs remains unaffected |
| Postgres concurrent replica claims | Disjoint claimed items, fenced stale results, no oversubscribed job budget; exercise SKIP LOCKED with separate connections |
| Database migration parity | Populated SQLite AutoMigrate and hand-written Postgres upgrade preserve old runs and establish equivalent constraints/indexes |
| Phase 1a binary against pre-jobs Postgres schema | Existing runs can still be created/read/updated; no SQL references unmigrated job columns; jobs return unsupported |
| Temporal replay/restart/lost wake-up | Driver rereads main-store state; history contains no mirrored item list/results; accepted progress survives driver replacement |
| Legacy listings with opted-in jobs in the same app | Default ListRuns/SearchRuns and pagination omit job children; explicit authorized child listings work; internal recovery still sees children |
| Run SSE and broad legacy callbacks | No job events/child deltas on parent run stream or default callbacks; explicit job stream/summary destination receives job progress |
| Duplicate/out-of-order native and engine usage notifications | Both only trigger a read of persisted native checkpoint usage; one transactional watermark prevents double counting; missing usage remains uncertain |
| Runtime dataset publication | Accepted item versions produce one canonical artifact; failed publication preserves prior artifact; retry is idempotent |
| Host validator configuration | Only app-configured endpoint/token used; model-supplied endpoints rejected; auth and revision binding tested |
| Crash before/after claim, run creation, tool result and result acceptance | Recover existing work; no double acceptance or lost committed progress |
| Old worker resumes after lease expires | Stale commit rejected; effects reconciled before reassignment |
| Model timeout, overload, rate limit or invalid output | Classified bounded retry/backoff; completed items untouched |
| Credential expiry and a user correction during recovery | Correct reply delivered once; authorization refreshed; revision enforced |
| Child returns “done” with missing records or false counts | Validation rejects completion |
| Retry after an external write with unknown outcome | Reconcile or block; never blindly repeat the effect |
| Four workers share a memory bank | Authorized recall succeeds; evidence remains attributable; no queue coordination through recall |
| Concurrent browser tasks | No shared-profile collision; unsupported concurrency serialized |
| Pause/cancel while children run or wait for approval | Dispatch stops, child state settles, no orphan work; accepted artifacts retained |
| Aggregate budget exhaustion | No new work admitted; reservations and in-flight usage visible; resume requires explicit budget policy |
| Scope/schema correction mid-job | Stale outputs rejected; affected items revalidated; unrelated progress preserved |
| Memory unavailable or consolidation delayed | Job remains recoverable and continues when memory is optional |
| Local sleep/restart and deployed worker replacement | Correct recovery; UI distinguishes unavailable compute from active processing |
| Multi-day soak and growing workflow history | Bounded context/history/storage, stable counters and no leaked resources; any driver rollover carries only identifiers and rediscovers state from the database |
| Crash between terminal status write and completion sequence | Impossible by construction: both commit in one transaction; fault-injected crash before commit leaves the member non-terminal, after commit leaves both written |
| Crash after commit, before cleanup/event emission | Completion is visible via cursor; recovery finishes idempotent cleanup; callbacks may arrive late or twice |
| Concurrent terminal transitions in one group (Postgres, separate connections) | Sequences are gap-free and commit-ordered per group; a cursor reader never skips a member that commits later |
| Every terminal path (complete, fail, cancel, blocked, recovery-failed) for a group member | Each produces exactly one completion sequence; ungrouped legacy runs unchanged |
| Host-owned group without a work job | Group create, member start, cancel cascade, budget and completions cursor work with `run_groups` only; members keep normal listing and callbacks |
| Host consumption crash tests (Helpin pilot) | Crash before host commit replays the completion; after commit, replay is a no-op; sweeper racing a completion advances the step once |
| Missed/duplicate callbacks with cursor pull | Cursor pull alone yields every completion once per host transaction; callbacks only reduce latency |
| Helpin pilot flow on run groups (phase 1b) | Plan advancement, cancellation and budgets match the legacy path; sweeper backstop records no required interventions |
| Existing non-job runs from Rex/Helpin/Usermaven | Existing API, tools, approvals, scheduling and memory-off behavior unchanged |

Extend `internal/api/openapi_contract_test.go` to document jobs capability, optional linkage, listing filters and separate job streams. Add a behavioral `TestJobsDisabledPreservesLegacyRunContract` alongside the API tests: an authenticated app without jobs capability receives HTTP 404 from job endpoints, while existing run creation/list/search/SSE/callback behavior remains unchanged. Run recorded Go SDK v0.7.0 and Usermaven Python streaming fixtures against this surface; tolerant parsing alone is not the compatibility gate.

Use fake providers and local fixture sites for deterministic fault tests; then a small authorized real-site pilot with source-quality review. Record coverage, duplicate attempts/effects, cost, throughput, retry counts and sampled accuracy. Run the multi-day soak before claiming multi-day reliability.

## 13. Compatibility and rollout

- New job behavior is opt-in; do not reinterpret every run as an autonomous job.
- Apps without jobs capability receive HTTP 404 from `/v1/work-jobs` endpoints. Capability negotiation is authoritative; SDK fallback handling of 404 cannot distinguish old servers from disabled apps. A backend without work-job support (including default `driver=memory`) advertises unavailable and returns a documented unsupported response for an opted-in app. Existing `/v1/runs` behavior, where mounted under `/v1`, and legacy event delivery remain unchanged for non-job runs. `/v1/memory/jobs` retains its existing meaning and contract.
- Keep existing start/resume/cancel and `finish_turn` contracts intact. New fields are optional for legacy callers; migrations are additive.
- Enforce app/actor authorization on jobs, items, child runs, artifacts and memory. Parent-supplied identifiers alone are not authority.
- Publish additive SDK contracts first where runtime imports SDK types, then runtime implementation and capability advertisement, then Rex integration. Compatibility and adoption are separate: existing integrations must keep working unchanged with Helpin's SDK v0.7.0 and Usermaven's HTTP client, and no app is forced to upgrade to stay compatible. An app adopting run groups or work jobs uses the newer SDK, or an explicitly chosen direct HTTP integration. Rex upgrades its SDK when adopting work jobs; Helpin upgrades only for its pilot flow. Avoid circular release dependencies.
- Phase 1a is SQLite/lightweight and Rex-only, with no Postgres DDL or Temporal changes. Phase 1b starts only when Helpin commits to a pilot flow. Do not enable deployed jobs until phase 1b migrations, replica claims and legacy contract tests pass.
- Helpin adopts per flow behind a flag, keeping existing orchestration and its stall sweeper as a backstop until the runtime completion signal has a production record. No host is required to migrate existing orchestration. Feature-disabled apps perform no job queries/dispatch, including on a store without job tables.
- Feature-gate sequential jobs initially, then parallelism, then adaptive delegation. Older runtimes must fail unsupported job requests clearly rather than silently running one batch.
- Test deployment shutdown, rolling upgrades and downgrade behavior with in-flight jobs. Do not silently discard jobs when the dispatcher is disabled; expose them as paused/unavailable for a compatible runtime to resume.
- Release runtime through the existing CI/staging/production process. Pin Rex to the verified tag and run macOS/Windows installer checks. Do not require Helpin or Usermaven to enable jobs to remain compatible.

## 14. Explicit deferrals

No general DAG editor or DAG engine, second orchestration framework, orchestration logic in the SDK, host domain logic (CRM, playbooks, command-bar plan semantics) in the runtime, unrestricted recursive agent tree, shared live transcript, automatic core-prompt rewriting, cross-machine browser migration, or blanket exactly-once claim for third-party effects. These are not needed to deliver durable sequential and bounded parallel jobs.

## 15. Research basis

Primary sources reviewed in the preceding architecture discussion:

- [Anthropic: Effective harnesses for long-running agents](https://www.anthropic.com/research/effective-harnesses-for-long-running-agents) — incremental sessions, explicit requirements and progress artifacts; compaction alone is insufficient.
- [Anthropic: Harness design for long-running application development](https://www.anthropic.com/research/harness-design-long-running-apps) — planner/generator/evaluator roles, explicit acceptance criteria and model-dependent context strategies. Coding results do not establish arbitrary research reliability.
- [LangGraph persistence](https://docs.langchain.com/oss/python/langgraph/persistence) — thread checkpoints and cross-thread stores serve different purposes.
- [Temporal Durable AI](https://docs.temporal.io/ai) — durable workflows, processing pipelines, failure recovery, waits and bounded activities.

These support the architecture direction. The phase gates and acceptance tests above determine whether our implementation actually meets the promised behavior.
