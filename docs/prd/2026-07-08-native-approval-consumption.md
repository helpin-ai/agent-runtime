# PRD — Native SDK approval consumption (mutating tool calls execute after approval)

- **Status:** Draft / proposed — **rev 3** (transcript-driven reconciliation keyed on persisted `tool_call_id`; adds the missing replay branch, concrete idempotency plumbing, at-least-once stance)
- **Author:** Azhar (azhar@d4interactive.io), with Claude
- **Date:** 2026-07-08
- **Component:** `agent-runtime` — native SDK executor (`internal/runtime`, `internal/engine`)
- **Severity:** High — silent data loss (run reports success, the mutation never happens)
- **Affects:** all apps using the shared runtime (helpin, usermaven, future apps) for any agent whose `approval_mode != "never"`

---

## 1. Summary

When a Native SDK agent calls a **mutating** tool (e.g. `create_document`), the runtime correctly pauses for human approval. But after the human approves, the runtime **re-requests approval instead of executing the tool**, and loops. The mutation is never performed. The run eventually ends `status=completed`, `approval_state=approved` — so it *looks* successful while having silently done nothing.

This PRD specifies making an approved tool call **actually execute and never be re-gated on durable re-entry** (with true exactly-once completed by a downstream idempotency key — see §4/§5.5).

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

### 5.1 Chosen approach — reconcile the recorded pending tool call on resume (transcript-driven)

> **Revised after review (rev 3).** Rev 1 keyed on a flat `map[sig]decision`; rev 2 added a FIFO occurrence queue by signature. Both fight the fact that `ExecuteRunActivity` re-invokes the **live LLM** on every attempt (`native_exec.go:161`), so a derived signature/occurrence-index can't reliably tell "replay the just-approved call" from "a new identical call." The robust key is not a derived signature — it is the **`tool_call_id` already persisted in the transcript**.

**Facts from the code:**
- When a mutating call needs approval, the loop appends a tool-result message whose output is the `approval_required` JSON (`native_exec.go:213-225`) and records a tool-call row with `approval_required=true` (`native_exec.go:227`), then pauses.
- On resume, `nativeResumedMessages` (`native_resume.go:30`) rebuilds history from `run.OutputSummary.Messages` (which contains that assistant tool-call turn and the `approval_required` result), appends the resume message, and re-invokes the LLM — hence the re-emit/re-gate loop.
- The transcript's tool-result block carries the **original `tool_call_id`** (`native_exec.go:216-218`). Read back from `OutputSummary`, it is **stable across resume**, unlike a fresh completion.

**Design:** reconcile the recorded pending call during history reconstruction, *before* the LLM sees it — extend `nativeResumedMessages` (or a step right after it). For each recorded tool-result block whose tool-call row has `approval_required=true`, resolve its decision via the persisted `tool_call_id`→interaction link (§5.3):

1. **approve, not yet executed** → execute the tool for real **once**; rewrite that block's output in the transcript to the real result; update the tool-call row (`approval_required=false`, real output, `consumed_at`, `interaction_id`). The LLM now sees the real result and continues — it does not re-emit.
2. **request_changes** → rewrite the block's output to the reviewer feedback; mark consumed; do not execute.
3. **already executed** (tool-call row now `approval_required=false` with a real output) → the transcript already carries the real result; **replay it, do nothing.** *This is the branch rev 2 was missing: a later activity retry finds the recorded real output and neither re-gates nor re-executes.*
4. a *new* mutating call the LLM emits later in the loop has a **new `tool_call_id`** with no prior record → it gates normally and gets its own approval.

Correlation is by the **persisted `tool_call_id`/tool-call row**, stable because it is read from storage, never re-derived. Branch 3 (same recorded `tool_call_id`) is cleanly distinct from branch 4 (new `tool_call_id`) — resolving the review's core "distinguish replay from new occurrence" gap with no determinism assumption about the LLM.

### 5.2 Why this over the signature-ledger

The signature approaches (rev 1/2) required a determinism assumption the runtime can't honor (a live LLM per activity attempt). The transcript already persists exactly what's needed — the assistant turn, the tool call, its `tool_call_id`, and the paused result — and is already reloaded on resume, so reconciling it in place is both smaller and correct. The earlier "execute the pending call on resume" idea (previously deferred as a big refactor) *is* this design; it is localized because `nativeResumedMessages` already reconstructs the transcript.

### 5.3 Persistence — link interaction↔tool-call by `tool_call_id`, mark consumed

The correlation and one-shot markers are explicit rows, not derived state:

- **Record `tool_call_id` on the approval interaction** at pause time (`nativeRequestToolApproval`, `native_interaction.go:304`) so the transcript's tool-result block links to its interaction. This is the stable join key (§5.1).
- **Add `consumed_at TIMESTAMPTZ NULL` to `agent_run_interactions`** — set once the authorized execution (or change-request delivery) is recorded. Authoritative one-shot marker; covers approve *and* request_changes (the latter writes no side effect, so a completed tool-call row alone wouldn't mark it).
- **On execution, update the existing tool-call row** for that `tool_call_id` in place (`approval_required=false`, real output) and set `interaction_id` — rather than appending a second row — so branch 3 (§5.1) sees a single, real record on replay.
- **Migration parity (hard requirement):** the Postgres path runs only `MigratePostgres` (raw SQL); `AutoMigrate` runs only for sqlite. Any new column (`consumed_at`, `interaction_id`, `tool_call_id` if not already stored) MUST be added in **both** the `CREATE TABLE` and an `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` in `MigratePostgres`, exactly as the `runtime_message_id` fix (`2a6400e`). Omitting the `ALTER` breaks every existing Postgres deployment.

`agent_run_tool_calls` remains the audit/replay ledger; the consume decision is gated on `consumed_at` + the row's `approval_required` flag, never on a bare signature.

### 5.4 Decision parsing + linking legacy interactions

**Decision parsing (from review).** The decision must be read from **both** the intent and the response payload, because the two approve paths differ:
- The runtime's own approve endpoint sends only `Intent: "approve"` with an empty `ResponsePayload` (`internal/api/server.go:457`); the stored fallback response payload is `{"intent":"approve","content":""}` (`internal/engine/engine.go:292`).
- Helpin's resume path additionally sends `response_payload: {"decision":"approve"}`.

So resolve: `decision := firstNonEmpty(responsePayload.decision, interaction-derived intent, run.last_resume.intent)`. Treat `approve`/`approved` as approve and `request_changes`/`reject`/`rejected` as change-request. Matching on `decision` alone would miss the normal runtime approve path.

**Legacy interactions self-heal (from review).** Interactions created *before* this change store `tool_name`+`input`/`raw_input` in `request_payload` but **no `tool_call_id`** (`internal/runtime/native_interaction.go:307`), so the §5.3 join key is missing. Fallback link: for a paused pre-change run there is exactly one trailing `approval_required` tool-result block and one pending (or just-resolved) interaction — link them **by position** (the single unresolved-then-resolved interaction ↔ the single pending tool-call). New runs use the explicit `tool_call_id` link; only in-flight legacy runs use positional linking, and only until they drain.

**Input canonicalization (still useful).** Where inputs are compared for safety, normalize `canonical_json(input)` (unmarshal→sorted-keys re-marshal) and tool name via `tools.CanonicalName`.

### 5.5 Downstream idempotency (required companion)

The side effect runs before the audit/consume row is committed (§4 stance), so a crash in that window lets a resume re-invoke the tool → possible double-write. Closing it needs an idempotency key honored at the `commands/execute` boundary. **This requires concrete plumbing that does not exist today:**

- `tools.CallContext` and `CommandExecutionContext` (`internal/tools/registry.go:22`) have **no** idempotency field — add one (e.g. `IdempotencyKey string`).
- `CommandExecutionRequest` (the HTTP body in `internal/tools/http_command_executor.go:34`) has no idempotency field — add it and send it as a header/body field on `POST /execute`.
- The native executor sets the key when executing an approved call: `IdempotencyKey = interaction_id` (stable across replays, unique per occurrence).
- **Helpin side:** `commands/execute` persists the key on first execution and, on a repeat, returns the prior result instead of re-mutating. (Recommend a dedicated `idempotency_key` envelope field over overloading `interaction_id`, so non-agent callers can use it too — see open questions.)

Without this the guarantee is **at-least-once**; runtime recovery reconciliation (on resume, treat a tool-call row already `approval_required=false` as executed — branch 3) shrinks the window but only the downstream key eliminates the duplicate-write on an ill-timed crash.

## 6. Implementation plan (phased)

**Phase 0 — schema**
- Additive migration: `consumed_at TIMESTAMPTZ NULL` + `interaction_id`/`tool_call_id` link columns on `agent_run_interactions` / `agent_run_tool_calls` as needed, in **both** `CREATE TABLE` and `MigratePostgres` `ALTER` (§5.3).

**Phase 1 — link plumbing**
- `nativeRequestToolApproval` (`native_interaction.go:304`): persist `tool_call_id` on the approval interaction (§5.3).
- Add a loader that, for a run, maps each recorded `approval_required` tool-call (by `tool_call_id`) to its interaction + resolved decision (parsed per §5.4), with the positional fallback for legacy rows.

**Phase 2 — resume reconciliation**
- In `nativeResumedMessages` (`native_resume.go:30`) — or a reconcile step invoked right after it — implement the 4-branch decision (§5.1): execute-approved-once (rewrite transcript output, update tool-call row, set `consumed_at`), deliver request_changes feedback, replay already-executed, leave genuinely-new calls to the normal gate.
- Add an in-activity guard against double execution within one pass. Pass the idempotency key (`interaction_id`) through the new `commands/execute` field (§5.5).

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
- Drive `Engine.ResumeRun` with an `approve` payload against a paused run and assert the downstream tool executor performs the side effect once on first resume, and that a second `ExecuteRunActivity` attempt (simulated retry) performs it **zero** more times (branch 3 replay) — true no-duplicate under crash is covered by the downstream idempotency-key test, not the runtime alone.

**Manual (staging)**
- Re-run the Competitive Intelligence Digest agent; approve the `create_document` gate once; assert a row appears in `docs_documents` and the agent receives a real document id/url.

## 8. Rollout & backward compatibility

- Pure runtime change; ships as a normal `agent-runtime` image bump (stage `rc.N` → prod).
- Backward compatible: agents with `approval_mode="never"` and read-only tools are unaffected. In-flight paused runs created before the change **do not carry the `tool_call_id` link** on their interactions; they reconcile via the positional fallback in §5.4 (single trailing pending tool-call ↔ single interaction) until they drain. Do not assume old rows carry the explicit link.
- The `consumed_at` column is additive and idempotent (see §5.3) — but only if added to **both** `MigratePostgres` (raw `ALTER`) and the `CREATE TABLE`; no data backfill required (NULL = not consumed, correct default).

## 9. Risks & mitigations

- **Suppressing a valid repeat mutation** (round-1 finding) → correlation is by persisted `tool_call_id`, not signature; a new identical call has a new `tool_call_id` and gates on its own (§5.1 branch 4).
- **No replay branch for an executed call** (round-2 finding) → §5.1 branch 3: on retry the tool-call row is already `approval_required=false` with the real output recorded in the transcript, so it replays and neither re-gates nor re-executes.
- **Double execution across a crash** (round-1 finding) → the side effect precedes the row commit, so the runtime alone is at-least-once. Branch-3 reconciliation shrinks the window; the downstream idempotency key (§5.5) closes it. PRD claims runtime non-re-gating/replay + downstream idempotency, not runtime exactly-once.
- **Missing the normal approve path** (the review's finding) → parse `intent` *and* `response_payload.decision` (§5.4); the runtime approve endpoint sets only `intent`.
- **Legacy in-flight interactions without the `tool_call_id` link** (round-2 finding) → link positionally (single trailing `approval_required` tool-call ↔ single interaction) until pre-change runs drain (§5.4); new runs carry the explicit link.
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
4. Confirm the executor pauses on the **first** mutating gate in a batch (so at most one trailing `approval_required` tool-call awaits reconciliation per resume). `canExecuteNativeToolCallsInParallel` forces mutating calls serial; verify a multi-mutation round can't leave two `approval_required` rows without distinct `tool_call_id`s. If it can, the `tool_call_id` link already disambiguates — the positional legacy fallback (§5.4) is the only path that assumes a single pending call.

## 12. Appendix — key code references

- Gate: `internal/runtime/native_interaction.go:292` (`nativeRequiresApproval`), `:304` (`nativeRequestToolApproval`)
- Tool execution: `internal/runtime/native_exec.go:799` (`executeSingleNativeToolCall`), `:812` (gate branch), `:833` (real execute)
- Resume: `internal/engine/engine.go:210` (`ResumeRun`), `:269` (`resolvePendingInteraction`)
- Ledger: `agent_run_tool_calls` store (`internal/store/gorm.go`, `AppendToolCall`)
- Tool-call identity: `NativeBlock.ToolCallID` / `ToolName` / `Input` (`internal/runtime/native_exec.go:87`)
- Prior migration-parity incident: commit `2a6400e` (runtime_message_id)
