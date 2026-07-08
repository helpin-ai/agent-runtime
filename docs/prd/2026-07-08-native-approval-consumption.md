# PRD — Native SDK approval consumption (mutating tool calls execute after approval)

- **Status:** Draft / proposed
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
- An approved mutating tool call executes **exactly once**, performing the real side effect (the `commands/execute` write).
- After execution, durable re-entry (Temporal replay / subsequent resume) **does not** re-gate or re-execute that call — it returns the recorded result.
- `request_changes` returns the reviewer's feedback to the agent as the tool result **without** executing the side effect.
- Fix is transparent to apps — no helpin/usermaven changes required; no app-config changes.
- No regression for `approval_mode = "never"` agents (which already work) or for read-only tools.

**Non-goals**
- Changing the human-facing approval UX in helpin/usermaven.
- Reworking the Codex approval path.
- Adding per-tool granular approval policies (all mutating tools gated) — that's a separate enhancement.
- Introducing an idempotency contract on the downstream `commands/execute` endpoint (helpin side) — desirable defense-in-depth, tracked separately.

## 5. Design

### 5.1 Chosen approach — persisted tool-call ledger + one-shot approval consumption

Leverage the existing `agent_run_tool_calls` store (already records `tool_name`, `input`, `output`, `mutating`, `approval_required`). Make tool execution **idempotent across durable re-entries** and make approval **consumable**.

Define a stable **tool-call signature** that survives LLM re-emission across replays:

```
sig = sha256(canonical_tool_name + "\x00" + canonical_json(input))
```

(`tool_call_id` cannot be used for cross-resume correlation — a fresh LLM completion after resume produces a new id. The stable identity is semantic: tool + normalized input.)

Execution decision in `executeSingleNativeToolCall`, for a `mutating` tool on an approval-mode agent:

1. **Already executed?** If a completed `agent_run_tool_calls` record exists for `sig` with `approval_required=false` (i.e. it ran), return its stored output. → deterministic replay, no double-write.
2. **Approved and pending execution?** If there is a `resolved` approval interaction for this run whose `response_payload.decision == "approve"` and whose recorded `sig` matches, and it has **not** been consumed: execute the tool for real, record the completed tool-call (with output, `approval_required=false`), mark the approval **consumed**. Return the real output.
3. **Change requested?** If the matching resolved interaction has `decision == "request_changes"`: return the reviewer content as the tool result (`is_error=false`, text = feedback), mark consumed, do **not** execute.
4. **Not yet approved?** Create the approval interaction (as today), recording `sig` and `tool_call_id` in the request payload, and pause.

"Consumed" is tracked so a later durable replay does not re-run step 2. Options (see 5.3): a boolean on the interaction, or the presence of the completed tool-call record from step 1 (preferred — no schema change).

### 5.2 Why not "execute the pending call on resume without re-running the LLM"

A cleaner-sounding alternative is: on pause, persist the exact pending tool call; on resume, execute it directly and append the result, then continue the loop — never re-running the model to re-derive the call. This is architecturally tidy but requires restructuring the native loop's resume entto "resume mid-tool-batch," which is a larger change to the durable activity boundaries and interacts with parallel tool batches. The ledger approach (5.1) achieves the same guarantee (execute-once, no re-gate) with a localized change and, as a bonus, makes **all** tool execution idempotent under replay. We therefore choose 5.1 and note 5.2 as a possible future refactor.

### 5.3 Persistence

Preferred: **no schema change.** Use `agent_run_tool_calls` as the ledger:
- On real execution (step 2), write a tool-call record with `approval_required=false` and the output. Its existence is the "consumed + executed" marker for step 1.
- Record `sig` either in a dedicated column or by recomputing it from the stored `tool_name`+`input` at read time (recompute avoids a migration).

If a dedicated marker proves cleaner, add a nullable `consumed_at TIMESTAMPTZ` to `agent_run_interactions` via an additive migration (both `CREATE TABLE` default and an `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` in `MigratePostgres`, mirroring the `runtime_message_id` fix in `2a6400e`). **Reminder:** the Postgres path runs only `MigratePostgres` (raw SQL); `AutoMigrate` runs only for sqlite. Any column added for this feature MUST be added in both places or Postgres deployments will break.

### 5.4 Signature normalization

`canonical_json(input)`: unmarshal to `map[string]any`, re-marshal with sorted keys, trimmed. Guards against key-order or whitespace differences between the approval-time input and the resumed-execution input. Tool name via `tools.CanonicalName`.

## 6. Implementation plan (phased)

**Phase 1 — correlation plumbing**
- `internal/runtime/native_exec.go`: compute `sig` for each tool call; include `sig` and `tool_call_id` in the approval interaction request payload built by `nativeRequestToolApproval` (`native_interaction.go`).
- Add a helper `nativeApprovedToolDecisions(execCtx) map[sig]decision` that reads resolved, unconsumed approval interactions for the run from `execCtx.Store` and the tool-call ledger.

**Phase 2 — gate rewrite**
- Rewrite the `mutating && nativeRequiresApproval(...)` branch in `executeSingleNativeToolCall` to implement the 4-way decision in §5.1 (replay / execute-approved / request-changes / request-approval).
- Ensure one-shot consumption: writing the completed tool-call record is the consume marker; guard against double execution within a single activity invocation with an in-memory set too.

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
- Backward compatible: agents with `approval_mode="never"` and read-only tools are unaffected. In-flight paused runs created before the change: on resume they take the new path (they have a resolved approval interaction with matching `sig`), so they self-heal.
- If a `consumed_at` column is added, it is additive and idempotent (see §5.3); no data backfill required.

## 9. Risks & mitigations

- **Double execution** if signature match is too loose or consumption isn't atomic → one-shot consume via the completed tool-call record + in-activity guard; execute-once asserted by tests.
- **Missed match** if input differs between approval and execution (model paraphrases input) → canonicalize input; if a mismatch still occurs the worst case is a re-prompt for approval (current behavior), not a wrong write.
- **Two legitimately-identical mutating calls** → each gets its own approval interaction; consumption is per-interaction, so the second still requires its own approval.
- **Migration on Postgres** (only if a column is added) → must touch both `MigratePostgres` and the `CREATE TABLE`, per the `runtime_message_id` incident.
- **Parallel mutating tool batch** → `canExecuteNativeToolCallsInParallel` already forces mutating calls to run serially; each is gated/consumed independently.

## 10. Metrics / observability

- Count of `approval.requested` vs `approval.executed` per run — a healthy run should not show N requests ≫ executes for the same `sig` (the bug's signature).
- Alert if a run completes with unconsumed `approve` interactions (indicates the mutation was approved but never executed).

## 11. Open questions

1. Confirm the Codex path consumes approvals correctly (spot-check a codex mutating run) — if not, a parallel fix is needed there.
2. Should the downstream helpin `commands/execute` also enforce an idempotency key (defense-in-depth against double writes)? Recommended but separable.
3. Preferred consume marker: reuse the tool-call ledger (no migration) vs. add `consumed_at` to interactions. Lean ledger-only.

## 12. Appendix — key code references

- Gate: `internal/runtime/native_interaction.go:292` (`nativeRequiresApproval`), `:304` (`nativeRequestToolApproval`)
- Tool execution: `internal/runtime/native_exec.go:799` (`executeSingleNativeToolCall`), `:812` (gate branch), `:833` (real execute)
- Resume: `internal/engine/engine.go:210` (`ResumeRun`), `:269` (`resolvePendingInteraction`)
- Ledger: `agent_run_tool_calls` store (`internal/store/gorm.go`, `AppendToolCall`)
- Tool-call identity: `NativeBlock.ToolCallID` / `ToolName` / `Input` (`internal/runtime/native_exec.go:87`)
- Prior migration-parity incident: commit `2a6400e` (runtime_message_id)
