# PRD — Native SDK approval consumption (mutating tool calls execute after approval)

- **Status:** Draft / proposed — **rev 2** (incorporates code-review findings: per-occurrence correlation, honest exactly-once stance, intent+decision parsing, sig-recompute self-heal)
- **Author:** Azhar (azhar@d4interactive.io), with Claude
- **Date:** 2026-07-08
- **Component:** `agent-runtime` — native SDK executor (`internal/runtime`, `internal/engine`)
- **Severity:** High — silent data loss (run reports success, the mutation never happens)
- **Affects:** all apps using the shared runtime (helpin, usermaven, future apps) for any agent whose `approval_mode != "never"`

---

## 1. Summary

When a Native SDK agent calls a **mutating** tool (e.g. `create_document`), the runtime correctly pauses for human approval. But after the human approves, the runtime **re-requests approval instead of executing the tool**, and loops. The mutation is never performed. The run eventually ends `status=completed`, `approval_state=approved` — so it *looks* successful while having silently done nothing.

This PRD specifies making an approved tool call **execute exactly once** and never be re-gated on durable re-entry.

## 2. Problem statement & evidence

Observed on staging, run `run_05d842d17958ca410e7c3a70` (helpin app, agent `Competitive Intelligence Digest`, Native SDK, interactive):

- The agent produced a plan, created a **space** and **collection**, then called `create_document` to write the report.
- `create_document` was attempted **9 times**. The write callback into helpin (`POST /api/internal/agent-runtime/commands/execute`) fired only **once** across the whole run.
- Two `approval_request` interactions for `create_document` were created and both marked `resolved` (approved) by the user.
- `docs_documents` contains **0 rows** for the target collection / report title. The collection exists; the document does not.
- The run finished `status=completed`, `approval_state=approved`, `max_tool_steps_reached=false` — i.e. it terminated "successfully" without ever writing the document.

**User-visible symptom:** "The document creation was approved; the tool did not return a document ID/URL." The agent misread the runtime's `approval_required` response as a successful (empty) result.

## 3. Root cause

Two facts combine into the loop:

1. **The approval gate is stateless.** `nativeRequiresApproval` (`internal/runtime/native_interaction.go:292`) is a pure function of the agent's `ApprovalMode`:

   ```go
   func nativeRequiresApproval(execCtx *ExecutionContext) bool {
       switch strings.TrimSpace(execCtx.Agent.ApprovalMode) {
       case "", agentcore.ApprovalModeNever:
           return false
       default:
           return true   // always true for an approval-mode agent
       }
   }
   ```

   It has no notion of "this specific tool call was already approved." A grep of `internal/runtime` for any approved / pre-approved / consume tracking returns nothing.

2. **Durable execution re-runs the LLM loop from the transcript on every resume.** `ResumeRun` (`internal/engine/engine.go:210`) resolves the pending interaction, sets `run.ApprovalState = approved` and `run.Input.Metadata["last_resume"]`, then re-enters `ExecuteRunActivity`. The executor re-runs the model, the model re-emits `create_document`, and `executeSingleNativeToolCall` (`internal/runtime/native_exec.go:812`) hits the gate again → `nativeRequestToolApproval` → new pending interaction → pause. The write branch (`execCtx.Tools.Execute`) is never reached.

The approval decision *is* persisted (`run.ApprovalState`, resolved interaction with `response_payload.decision=approve`, `last_resume`), but the native executor never consults it to let the approved call through.

**Scope note:** This is specific to the **Native SDK** path. The Codex path handles approvals through the codex app-server protocol (`internal/runtime/codex_event_mapper.go`) and is out of scope here (to be verified — see Open Questions).

## 4. Goals / Non-goals

**Goals**
- An approved mutating tool call **executes and returns its real result to the agent** (the `commands/execute` write actually happens), instead of being re-gated into a silent no-op loop.
- After the approved call has executed, durable re-entry (Temporal replay / a subsequent resume for the *next* gate) **does not** re-gate or re-run that already-approved occurrence.
- Each distinct occurrence of a mutating call requires its own approval — approving one occurrence must NOT auto-satisfy a later, semantically-identical call.
- `request_changes` returns the reviewer's feedback to the agent as the tool result **without** executing the side effect.
- Fix is transparent to apps — no helpin/usermaven changes required; no app-config changes.
- No regression for `approval_mode = "never"` agents (which already work) or for read-only tools.

**Execution-guarantee stance (revised after review):** The runtime alone can only provide **at-least-once** execution of the side effect. The external tool runs (`native_exec.go:833`) *before* the audit row is written (`AppendToolCall`, a plain insert — `internal/store/gorm.go:515`), so a crash in that window means a resume can re-invoke the side effect. **True exactly-once therefore requires an idempotency key honored by the downstream `commands/execute` boundary** (helpin). This PRD scopes the runtime to at-least-once + non-re-gating, and makes the downstream idempotency key a **required companion change** (see §5.5), not an optional extra.

**Non-goals**
- Changing the human-facing approval UX in helpin/usermaven.
- Reworking the Codex approval path.
- Adding per-tool granular approval policies (all mutating tools gated) — that's a separate enhancement.

**Required companion (not a non-goal anymore):** an idempotency key on helpin's `commands/execute` so retries after a crash don't double-write. Without it the guarantee is only at-least-once. See §5.5.

## 5. Design

### 5.1 Chosen approach — per-approval-occurrence correlation (not a flat sig map)

> **Revised after review.** The first draft matched a flat `map[sig]decision` and replayed the stored output for *any* completed call with the same signature. That is wrong for mutations: it conflicts with the goal that two legitimately-identical mutating calls each require approval — the second call would silently replay the first's output instead of gating. The corrected design correlates each execution to a **specific pending/approved interaction occurrence**, not to "any historical call with this signature."

We use the signature only to **locate a pending approval occurrence**, and we track consumption against that **specific interaction's ID**. Multiple occurrences of the same signature form a **FIFO occurrence queue**: the Nth pending-then-approved interaction authorizes the Nth execution, and each is consumed exactly once.

Stable **signature** (used only for locating the matching pending occurrence, since a fresh LLM completion after resume yields a new `tool_call_id`):

```
sig = sha256(canonical_tool_name + "\x00" + canonical_json(input))
```

State, per run, built from `execCtx.Store` at the start of an execution pass:
- The ordered list of `approval_request` interactions for the run, each with: `id`, `status`, resolved decision, recorded `sig` (see §5.4 for the recompute fallback), and whether it has been **consumed** (§5.3).

Decision in `executeSingleNativeToolCall`, for a `mutating` tool on an approval-mode agent — matching against the **oldest unconsumed** interaction for this `sig`:

1. **Matching interaction is resolved = approve, not yet consumed** → execute the tool for real; write the completed `agent_run_tool_calls` row (output, `approval_required=false`) carrying the **consumed interaction ID** so the linkage is explicit; mark that interaction consumed. Return the real output.
2. **Matching interaction is resolved = request_changes, not yet consumed** → return the reviewer content as the tool result (`is_error=false`, text = feedback); mark consumed; do **not** execute.
3. **Matching interaction is still pending** (we are inside a Temporal replay of the same paused step) → re-emit the same pause without creating a *new* interaction (idempotent pause), so replay doesn't multiply pending rows.
4. **No unconsumed interaction for this `sig`** → this is a *new* occurrence: create a fresh approval interaction recording `sig` and `tool_call_id`, and pause.

Consumption is keyed to the **interaction ID / tool-call row**, never to the bare signature — so re-calling the same tool later cleanly falls to case 4 and gets its own approval.

### 5.2 Why not "execute the pending call on resume without re-running the LLM"

A cleaner-sounding alternative is: on pause, persist the exact pending tool call; on resume, execute it directly and append the result, then continue the loop — never re-running the model to re-derive the call. This is architecturally tidy but requires restructuring the native loop's resume entto "resume mid-tool-batch," which is a larger change to the durable activity boundaries and interacts with parallel tool batches. The ledger approach (5.1) achieves the same guarantee (execute-once, no re-gate) with a localized change and, as a bonus, makes **all** tool execution idempotent under replay. We therefore choose 5.1 and note 5.2 as a possible future refactor.

### 5.3 Persistence — the consumed marker must be explicit

A completed `agent_run_tool_calls` row is **not** a sufficient consume marker by itself (that was the flat-signature mistake — it can't tell "this occurrence" from "a future identical occurrence", and it doesn't cover the request_changes case which writes no side effect). Track consumption explicitly against the interaction:

- **Add `consumed_at TIMESTAMPTZ NULL` to `agent_run_interactions`.** An interaction is consumed once its authorized execution (or its change-request delivery) has been recorded. This is the authoritative one-shot marker and works for both approve and request_changes.
- Link the executed `agent_run_tool_calls` row to the interaction it consumed (store `interaction_id`), for audit and for the recovery check in §5.5.
- **Migration parity (hard requirement):** the Postgres path runs only `MigratePostgres` (raw SQL); `AutoMigrate` runs only for sqlite. The new column MUST be added in **both** the `CREATE TABLE` and an `ALTER TABLE ... ADD COLUMN IF NOT EXISTS ... consumed_at` statement in `MigratePostgres`, exactly as the `runtime_message_id` fix (`2a6400e`) had to. Omitting the `ALTER` breaks every existing Postgres deployment.

`agent_run_tool_calls` remains the audit ledger (and supports the at-least-once recovery reconciliation), but the **consume decision is gated on `consumed_at`**, not on the mere existence of a tool-call row.

### 5.4 Signature normalization + decision parsing + sig recompute

**Normalization.** `canonical_json(input)`: unmarshal to `map[string]any`, re-marshal with sorted keys, trimmed. Guards against key-order or whitespace differences between approval-time input and resumed-execution input. Tool name via `tools.CanonicalName`.

**Decision parsing (revised after review).** The decision must be read from **both** places, because the two approve paths differ:
- The runtime's own approve endpoint sends only `Intent: "approve"` with an empty `ResponsePayload` (`internal/api/server.go:457`); the stored fallback response payload is `{"intent":"approve","content":""}` (`internal/engine/engine.go:292`).
- Helpin's resume path additionally sends `response_payload: {"decision":"approve"}`.

So the implementation reads: `decision := firstNonEmpty(responsePayload.decision, interaction-derived intent, run.last_resume.intent)`. Treat `"approve"`/`"approved"` as approve and `"request_changes"`/`"reject"`/`"rejected"` as change-request. Matching on `decision` alone would miss the normal runtime approve path.

**Sig recompute fallback (revised after review).** Interactions created *before* this change store `tool_name` + `input`/`raw_input` in `request_payload` but **no `sig` and no `tool_call_id`** (`internal/runtime/native_interaction.go:307`). When loading interactions, if `sig` is absent, **recompute it from `request_payload.input` (or `raw_input`)** using the same normalization. This is what actually makes in-flight paused runs self-heal — not an assumption that old rows already carry `sig`.

### 5.5 Downstream idempotency (required companion for exactly-once)

Because the side effect runs before the audit/consume row is committed (§4 stance), a crash in that window leads a resume to re-invoke the tool → potential double-write. To get true exactly-once:

- The runtime derives a stable **idempotency key** per authorized execution: `idem = interaction_id` (the consumed approval's ID — stable across replays, unique per occurrence).
- It passes `idem` on the `commands/execute` call to helpin.
- Helpin persists `idem` on first execution and, on a repeat with the same `idem`, returns the prior result instead of re-performing the mutation.

Without this, the guarantee is **at-least-once** and the failure mode is a duplicate document on an ill-timed crash — acceptable as an interim, but the idempotency key is the correct fix and is in scope as a companion helpin change. Runtime-side recovery reconciliation (on startup/resume, mark interactions whose linked tool-call row exists as consumed) reduces but does not eliminate the window; only the downstream key closes it.

## 6. Implementation plan (phased)

**Phase 0 — schema**
- Additive migration: `consumed_at TIMESTAMPTZ NULL` on `agent_run_interactions` and `interaction_id` link on `agent_run_tool_calls`, in **both** `CREATE TABLE` and `MigratePostgres` `ALTER` (§5.3).

**Phase 1 — correlation plumbing**
- `internal/runtime/native_exec.go`: compute `sig` per tool call; include `sig` and `tool_call_id` in the approval request payload built by `nativeRequestToolApproval` (`native_interaction.go`).
- Add `nativeApprovalOccurrences(execCtx)` returning, per `sig`, an **ordered queue** of interactions with `{id, resolvedDecision, consumed}`. Decision parsed per §5.4 (intent + decision); `sig` recomputed from `request_payload.input`/`raw_input` when absent (§5.4).

**Phase 2 — gate rewrite**
- Rewrite the `mutating && nativeRequiresApproval(...)` branch in `executeSingleNativeToolCall` to the 4-way decision in §5.1, matching the **oldest unconsumed** interaction for the `sig`.
- Consume by setting `consumed_at` on that interaction and linking the tool-call row; add an in-activity guard set against double execution within one pass. Pass `idem = interaction_id` to `commands/execute` (§5.5).

**Phase 3 — request_changes path**
- Map `decision == "request_changes"` to a tool result carrying the reviewer's `content`, so the model can revise rather than silently loop.

**Phase 4 — tests** (see §7).

**Phase 5 — observability**
- Structured logs at each branch (`approval.replayed`, `approval.executed`, `approval.change_requested`, `approval.requested`) with `run_id`, `tool_name`, `sig` prefix.
- Emit an event when an approved tool executes so helpin can surface "action performed."

## 7. Testing

**Unit (`internal/runtime/native_exec_test.go`)**
- Mutating tool, approval-mode agent, no prior approval → creates interaction, pauses, does not execute.
- Same tool after a resolved `approve` interaction for matching `sig` → executes once, records tool-call, returns real output; **second** pass (simulated replay) → returns stored output, does NOT execute again, does NOT create a new interaction.
- `request_changes` → returns feedback as tool result, no execution.
- `approval_mode="never"` → executes directly (unchanged).
- Read-only tool → never gated (unchanged).
- Input key-order/whitespace variance between approval and execution still matches `sig`.

**Integration**
- Drive `Engine.ResumeRun` with an `approve` payload against a paused run and assert the downstream tool executor performs the side effect exactly once.

**Manual (staging)**
- Re-run the Competitive Intelligence Digest agent; approve the `create_document` gate once; assert a row appears in `docs_documents` and the agent receives a real document id/url.

## 8. Rollout & backward compatibility

- Pure runtime change; ships as a normal `agent-runtime` image bump (stage `rc.N` → prod).
- Backward compatible: agents with `approval_mode="never"` and read-only tools are unaffected. In-flight paused runs created before the change **do not carry `sig`/`tool_call_id`** in their existing interaction payloads; they self-heal only because §5.4 recomputes `sig` from the stored `request_payload.input`/`raw_input` at read time. Call this out in implementation — do not assume old rows already have `sig`.
- The `consumed_at` column is additive and idempotent (see §5.3) — but only if added to **both** `MigratePostgres` (raw `ALTER`) and the `CREATE TABLE`; no data backfill required (NULL = not consumed, correct default).

## 9. Risks & mitigations

- **Suppressing a valid repeat mutation** (the review's finding) → never consume by bare signature; consume by interaction ID via a FIFO occurrence queue per `sig` (§5.1). A second identical call finds no unconsumed interaction and correctly gets its own approval.
- **Double execution across a crash** (the review's finding) → the ledger row is written *after* the side effect, so it cannot guarantee exactly-once alone. Mitigated to at-least-once by recovery reconciliation; closed only by the downstream idempotency key (§5.5). PRD no longer claims exactly-once from the runtime alone.
- **Missing the normal approve path** (the review's finding) → parse `intent` *and* `response_payload.decision` (§5.4); the runtime approve endpoint sets only `intent`.
- **Stale in-flight interactions without `sig`** (the review's finding) → recompute `sig` from `request_payload.input`/`raw_input` (§5.4); do not assume old rows carry it.
- **Missed match** if the model paraphrases input between approval and execution → canonicalize input; worst case is a re-prompt for approval (current behavior), not a wrong write.
- **Migration on Postgres** → must touch both `MigratePostgres` (`ALTER`) and the `CREATE TABLE`, per the `runtime_message_id` incident.
- **Parallel mutating tool batch** → `canExecuteNativeToolCallsInParallel` already forces mutating calls serial; each is matched/consumed against its own interaction, in order.

## 10. Metrics / observability

- Count of `approval.requested` vs `approval.executed` per run — a healthy run should not show N requests ≫ executes for the same `sig` (the bug's signature).
- Alert if a run completes with unconsumed `approve` interactions (indicates the mutation was approved but never executed).

## 11. Open questions

1. Confirm the Codex path consumes approvals correctly (spot-check a codex mutating run) — if not, a parallel fix is needed there.
2. Idempotency key on `commands/execute` is now **in scope** (§5.5). Open sub-question: does helpin want the runtime to send `interaction_id` as the key, or a dedicated `idempotency_key` field on the command envelope? (Recommend the latter for clarity.)
3. Consume-marker decision is **resolved**: explicit `consumed_at` on the interaction (ledger-row existence is insufficient — see §5.3).
4. FIFO occurrence matching assumes at most one pending occurrence per `sig` at a time (the executor pauses on the first gate). Confirm no path emits two pending same-`sig` gates concurrently; if it can, key the queue by emission order/`tool_call_id` recorded at pause time.

## 12. Appendix — key code references

- Gate: `internal/runtime/native_interaction.go:292` (`nativeRequiresApproval`), `:304` (`nativeRequestToolApproval`)
- Tool execution: `internal/runtime/native_exec.go:799` (`executeSingleNativeToolCall`), `:812` (gate branch), `:833` (real execute)
- Resume: `internal/engine/engine.go:210` (`ResumeRun`), `:269` (`resolvePendingInteraction`)
- Ledger: `agent_run_tool_calls` store (`internal/store/gorm.go`, `AppendToolCall`)
- Tool-call identity: `NativeBlock.ToolCallID` / `ToolName` / `Input` (`internal/runtime/native_exec.go:87`)
- Prior migration-parity incident: commit `2a6400e` (runtime_message_id)
