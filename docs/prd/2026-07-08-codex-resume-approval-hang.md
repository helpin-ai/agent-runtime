# PRD — Fix the codex resume-after-approval silent hang

- **Status:** Draft / proposed
- **Author:** Azhar (azhar@d4interactive.io), with Claude
- **Date:** 2026-07-08
- **Component:** `agent-runtime` — Codex app-server runtime (`internal/runtime/codex.go`, `codex_appserver_client.go`, `codex_pause.go`)
- **Severity:** High — every Codex run that requires an approval hangs after the user approves; the run freezes for ~30 minutes with no output, then the activity times out.
- **Affects:** Codex/Command-Agent runs (helpin, usermaven, future apps) that pause for a tool/exec approval or user-input request.

---

## 1. Summary

A Codex run's **first turn works** — it authenticates, runs tools, and pauses for approval. But the turn **immediately after the approval-resume hangs silently**: no events, no error, no progress, until the ~30-minute codex activity timeout fires. Root cause: on resume the runtime starts a **fresh codex app-server process** and then blocks indefinitely waiting for that process to **re-emit the pending approval request**, which a resumed process never does.

## 2. Evidence (staging, reproducible)

Two runs hung at exactly the same point:
- `run_4067…` — approved, then silent 8+ min, 0 events, cancelled.
- `run_df7e4a…` — clean repro: `run.started`(23:03) → tools ran (`list_mcp_resources`, `run_command`×2) → `run.paused` for `human_approval`(23:04) → user approved (interaction `resolved`) → `run.started`(23:06:12, resume) → `codex_auth.state_changed`(23:06:13) → **nothing**. `status=running`, `updated_at` frozen at 23:06:12; no worker events, no ERROR/WARN, for the whole window.

The signature: **first turn fine (same process), resume turn hangs (fresh process).**

## 3. Root cause (confirmed in code)

Each `ExecuteRunActivity` invocation starts a **new** codex app-server process (`codex.go:152` `newCodexAppServerClient` + `client.Start`). On a resume with a pending approval:

1. `startOrResumeCodexThread` issues `thread/resume` (`codex.go:174`, `:356-360`) — restores conversation history in the **fresh** process.
2. `state.PendingRequest != nil` → `respondToPendingCodexRequest` (`codex.go:180`, `:530`).
3. That calls `awaitPendingCodexRequestReplay` (`codex.go:534`, `:553`) **first**:

   ```go
   for {
       msg, err := client.Next(ctx)          // blocks on codex stdout
       if err != nil { return err }
       if isCodexPauseRequestMethod(msg.Method) { return msg, nil } // wait for replay
   }
   ```

4. It is waiting for the fresh process to **re-issue** the pending approval request (`item/commandExecution/requestApproval` / `item/fileChange/requestApproval` / `item/tool/requestUserInput` / `item/permissions/requestApproval` — `codex_pause.go:67`). A `thread/resume`'d fresh process **does not replay the in-flight paused turn's approval request** — that state lived in the original (now-dead) process. So no such message ever arrives.
5. `client.Next(ctx)` (`codex_appserver_client.go:216`) selects only on the stdout line channel and `ctx.Done()`. `awaitPendingCodexRequestReplay` has **no inner timeout**. So it blocks until the activity `ctx` deadline — the codex adapter's `cfg.Timeout`, defaulting to **30 minutes** (`NewCodexAdapterWithConfig`, `codex.go:66-70`).

That is the silent 30-minute hang. The `codex_auth.state_changed` at 23:06:13 is the last thing emitted (from `ensureCodexAuthenticated`); execution then enters the blocking replay-await and produces nothing.

**Why the process can't be kept alive across the pause:** runs are durable (Temporal) and can resume on a different worker/pod, so the codex app-server that issued the approval is necessarily gone by resume time. Any correct design must tolerate a fresh process on resume.

## 4. Goals / Non-goals

**Goals**
- After an approval-resume, the run either **continues deterministically** or **fails fast with a clear error** — never hangs waiting on a replay that won't come.
- The approval decision (`approve` / `request_changes`) reaches Codex and the run makes progress.
- A stuck codex turn is **visible** (logged) and **bounded** (heartbeat/timeout), not a silent 30-minute freeze.

**Non-goals**
- Keeping a codex process alive across pause/resume (impossible across pods — see §3).
- Changing the approval UX.
- The already-shipped auth loop / shared-store work (separate, done).

## 5. Design

### 5.1 Bound the replay wait (stop the silent hang)
Wrap `awaitPendingCodexRequestReplay` in a short `context.WithTimeout` (e.g. 10–20s) instead of the 30-minute activity ctx. If the fresh process does not re-emit the pending request within that window, stop waiting and take the fallback (§5.2) — the run never freezes for 30 minutes.

### 5.2 Fallback: deliver the decision as a new turn, not a reply to a phantom request
A fresh process has no in-flight request to `Respond` to. Instead of `client.Respond(pendingID, …)` against a request that was never re-issued:
- On **approve**: submit a **new turn** (`startCodexTurn`) instructing Codex to proceed with the previously-approved action (the code already computes a `followup`/continuation string — `codexResumeResponse`, `codex.go:542-543`), then `collectCodexTurn`. This makes progress deterministic and independent of replay.
- On **request_changes**: submit the reviewer feedback as the new turn input.
- If, within the §5.1 window, the process **does** re-emit the pending request (some codex versions/paths may replay), keep the existing `Respond` path — detect-and-use, fallback otherwise.

### 5.3 Heartbeat + logging (make hangs visible and fail-fast)
- Record a Temporal **activity heartbeat** around the codex await/collect loops with a heartbeat timeout far below 30 min, so a genuinely stuck turn fails fast and retries instead of blocking a worker slot silently.
- Log each resume step: `thread/resume ok`, `awaiting pending-request replay`, `replay received` / `replay timeout → new-turn fallback`, `turn collecting`. Today a hung resume emits **zero** logs.

## 6. Edge cases
- **request_user_input** (not just exec/file approvals) pauses go through the same `awaitPendingCodexRequestReplay`; the fix must cover all `isCodexPauseRequestMethod` kinds.
- **Multiple pending requests** in one turn (batch) — the new-turn fallback sidesteps per-request replay; verify Codex applies the batch given the continuation instruction.
- **approve vs request_changes** must both route through the fallback.
- **thread/resume failure** (thread expired) — surface a clear error, don't hang.

## 7. Testing
- **Unit:** a fake codex client that, after `thread/resume`, emits **no** pause-request → `awaitPendingCodexRequestReplay` returns within the timeout and the adapter takes the new-turn fallback (asserts `startCodexTurn` is called, no indefinite block).
- **Unit:** fake client that **does** replay the pending request → existing `Respond` path still used.
- **Timeout:** assert the await is bounded (does not use the 30-min ctx).
- **Integration/manual (staging):** run a Codex agent, approve the tool call, confirm the run **continues** (creates the doc/task) instead of freezing; confirm resume-step logs appear.

## 8. Risks & mitigations
- **Double execution** of the approved action (if Codex both replayed and we also sent a new turn) → detect-and-use replay when present; only fall back when the replay window elapses; make the continuation instruction idempotent-friendly (aligns with the native-path idempotency follow-up).
- **New turn misinterpreted** by Codex (it may re-plan rather than execute the exact approved action) → phrase the continuation explicitly ("the previously requested <tool/command> was approved; proceed with it"), and carry the original request details from `state.PendingRequest`.
- **Heartbeat tuning** too aggressive → set the heartbeat timeout with margin over normal turn latency.

## 9. Open questions
1. O1 — Does codex app-server `thread/resume` **ever** replay an in-flight `requestApproval`? If yes and reliably, prefer fixing the resume so replay happens; if never, the new-turn fallback (§5.2) is the primary path. Needs a protocol check against the pinned `@openai/codex` version.
2. O2 — Does `state.PendingRequest` carry enough detail (tool name, command, args) to phrase an unambiguous continuation instruction for the fallback?
3. O3 — Should the codex activity timeout (`cfg.Timeout`, default 30 min) also be lowered, or is the heartbeat sufficient?

## 10. Code references
- Resume/approval flow: `internal/runtime/codex.go:145-213` (`executeAppServer`), `:174` (`startOrResumeCodexThread`), `:179-197` (pending-request branch).
- The blocking await: `internal/runtime/codex.go:530` (`respondToPendingCodexRequest`), `:553` (`awaitPendingCodexRequestReplay`).
- Blocking read: `internal/runtime/codex_appserver_client.go:216` (`Next`).
- Pause request methods: `internal/runtime/codex_pause.go:67` (`isCodexPauseRequestMethod`).
- Continuation string: `codexResumeResponse` (`codex.go:542-543`).
- Activity timeout default: `NewCodexAdapterWithConfig` (`codex.go:66-70`).
