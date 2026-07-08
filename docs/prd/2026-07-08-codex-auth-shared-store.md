# PRD — Shared, durable Codex auth store (fix the device-code sign-in loop)

- **Status:** Draft / proposed — **rev 2** (concrete `cmd/*` wiring after `openStore`; store access decided as a standalone repo over `(*store.SQL).DB()`; selection rule = any SQL store + key)
- **Author:** Azhar (azhar@d4interactive.io), with Claude
- **Date:** 2026-07-08
- **Component:** `agent-runtime` — Codex auth (`internal/runtime/codex_auth*.go`, `codex.go`, `cmd/*/main.go`)
- **Severity:** High — Codex agents are unusable in a multi-pod deployment (infinite sign-in loop)
- **Affects:** all apps using Codex agents (helpin, usermaven, future apps) on any deployment where the API and worker are separate pods (i.e. staging and prod)

---

## 1. Summary

Codex device-code auth tokens are persisted to a **pod-local, unmounted, ephemeral directory** (`AGENT_RUNTIME_CODEX_AUTH_DIR`, e.g. `.local/codex-auth`). Device sign-in completes on the **API pod** (the auth manager) and writes the token there; the run executes on the **worker pod** and reads its **own empty** copy — so the token is never found and the run re-prompts. The user sees the ChatGPT sign-in screen come back immediately after every successful sign-in, forever. The token is also lost on any pod restart.

This PRD makes the Codex auth store **shared across pods and durable across restarts** by backing it with the runtime's existing Postgres store (encrypted at rest with the existing key), behind the existing `CodexAuthStore` interface.

## 2. Problem statement & evidence

Observed on staging after enabling `CODEX_OPENAI_AUTH_MODE=chatgpt_device_code`:

- The Command/Codex agent run pauses with a "Codex authentication required" interaction and the UI shows **"ChatGPT sign-in required"** (correct — no token yet).
- The user completes the ChatGPT device-code login; the screen disappears for ~1s and then **the sign-in screen returns**. Repeats indefinitely. `codex_auth.state_changed` events flap (required → … → required).

Infrastructure facts (verified on the bastion):

- `AGENT_RUNTIME_CODEX_AUTH_DIR = .local/codex-auth` — a **relative** path.
- **Neither** `agent-runtime` (API) nor `agent-runtime-worker` has any `volumeMounts`/`volumes` for it; the only PVC in the namespace is Postgres (`hcloud-volumes`, RWO).
- So each pod has its own empty, ephemeral `.local/codex-auth`.

Two-pod flow that produces the loop:

1. Run executes on the **worker** → `CodexAdapter.ensureCodexAuthenticated` → `account/read` reports auth required → creates the auth-required pause → UI shows **Start sign-in**.
2. **Start sign-in** hits the **API** pod → `CodexAuthManager.StartDeviceCode` issues the device code, `watchSession` waits; on completion it `promoteCodexAuth`s the token into **the API pod's** `.local/codex-auth` and emits `connected`.
3. Run resumes on the **worker** → `restoreCodexAuth` reads **the worker's own** `.local/codex-auth` (empty) → `account/read` still auth required → re-prompt. Loop.

## 3. Root cause

The `CodexAuthStore` used in every environment is `FileCodexAuthStore` (`internal/runtime/codex_auth.go`), which reads/writes a per-scope file under a **local directory**. In a multi-pod deployment the promote (API pod) and restore (worker pod) touch **different filesystems**, so the token never crosses pods. Even single-pod, the directory has **no volume**, so it does not survive a restart.

Note: the scope key is **not** the bug. `CodexAuthScope{AppID, TenantID, Provider, AuthMode}` (`codex_auth.go`) is derived identically by the manager (promote) and the adapter (restore) for a given app/tenant/provider/mode, so both address the same logical entry — they just address it in two unshared filesystems.

## 4. Goals / Non-goals

**Goals**
- Codex device-code tokens persist in a store **both** the API and worker pods read/write, and that **survives pod restarts**.
- One completed device sign-in unblocks subsequent runs (no re-prompt loop).
- Tokens remain **encrypted at rest**, reusing the existing 32-byte `AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY`.
- No change to the `CodexAuthStore` interface or its call sites — swap the implementation.
- No app (helpin/usermaven) change; config-only enablement.

**Non-goals**
- Changing the device-code UX / prompt flow.
- The `api_key` app-server auth gap (separate follow-up — codex app-server never gets `OPENAI_API_KEY` wired; see the earlier 401 investigation).
- Catch-mid-turn-401 → re-prompt robustness (separate follow-up).
- Multi-region token replication.

## 5. Design — Postgres-backed `CodexAuthStore`

Implement `StoreBackedCodexAuthStore` satisfying the existing interface:

```go
type CodexAuthStore interface {
    Restore(ctx, scope CodexAuthScope, codexHome string) error // store -> CODEX_HOME/auth.json
    Promote(ctx, scope CodexAuthScope, codexHome string) error // CODEX_HOME/auth.json -> store
    Clear(ctx, scope CodexAuthScope) error
}
```

Backed by a new table in the runtime DB (already shared by both pods and already holds runs/messages/interactions/tool-calls):

```sql
CREATE TABLE IF NOT EXISTS codex_auth_tokens (
    app_id      TEXT NOT NULL,
    tenant_id   TEXT NOT NULL DEFAULT '',
    provider    TEXT NOT NULL,
    auth_mode   TEXT NOT NULL,
    payload     BYTEA NOT NULL,          -- encrypted auth.json bytes
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (app_id, tenant_id, provider, auth_mode)
);
```

- **Promote**: read `CODEX_HOME/auth.json`, encrypt with the existing key (AES-256-GCM, same scheme as `EncryptedFileCodexAuthStore`), `UPSERT` by the scope PK, set `updated_at`.
- **Restore**: `SELECT payload` by scope PK; if present, decrypt and write `CODEX_HOME/auth.json` (0600); if absent, no-op (the adapter then prompts — correct).
- **Clear**: `DELETE` by scope PK (logout / auth failure).
- Encryption is mandatory for this store (refuse to construct without a 32-byte key) — tokens must never sit in the DB in plaintext.

**Store access (decided, not an open question).** `agentcore.Store` (`internal/agentcore/store.go:5`) deliberately exposes **no** DB handle, but the concrete SQL store does: `(*store.SQL).DB() *gorm.DB` (`internal/store/gorm.go:56`). Decision: implement a **standalone `codexAuthRepository`** over a `*gorm.DB` (own table + encryption). Do **not** widen the core `agentcore.Store` interface with codex-specific methods. `cmd/*` obtains the `*gorm.DB` by type-asserting the `agentcore.Store` returned by `openStore`:

```go
sqlStore, ok := persistentStore.(*store.SQL)   // ok == false for the memory store
```

### 5.1 Wiring / selection (revised after review — implementable as written)

`DefaultCodexConfigFromEnv` (`codex.go:48`) reads env only and **cannot** see the DB, so it must NOT choose the store. Keep it env-only (it still builds the file store from `AGENT_RUNTIME_CODEX_AUTH_DIR` as the local fallback). The store-backed auth store is attached in **`cmd/*` after `openStore`**, overriding `cfg.AuthStore`, **before** the adapter and manager are constructed. In both `cmd/agent-runtime/main.go` and `cmd/agent-runtime-worker/main.go` (`persistentStore` already exists before `codexConfig` — lines 30/35 and 31/61):

```go
persistentStore, _ := openStore(ctx)          // existing
codexConfig := runtime.DefaultCodexConfigFromEnv()  // existing (env-only)

if sqlStore, ok := persistentStore.(*store.SQL); ok {
    if key, err := runtime.ParseCodexAuthEncryptionKey(os.Getenv("AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY")); err == nil && len(key) == 32 {
        codexConfig.AuthStore = runtime.NewStoreBackedCodexAuthStore(sqlStore.DB(), key) // shared, durable
    }
}
// then, unchanged, both consumers get the same store:
//   runtime.NewCodexAdapterWithConfig(codexConfig)   (worker + api)
//   runtime.NewCodexAuthManager(persistentStore, codexConfig)  (api)
```

(Optionally encapsulate this as a helper `runtime.DefaultCodexConfigFromEnvWithAuthStore(store)` to keep both mains identical.) Because `codexConfig` is shared into both `NewCodexAdapterWithConfig` and `NewCodexAuthManager`, the adapter (worker) and manager (API) get the **same** store instance semantics (same DB, same table, same scope) — which is the whole point.

**Selection rule (tightened after review):**
- **DB-backed** whenever `persistentStore` is a `*store.SQL` (Postgres **or** sqlite) **and** a valid 32-byte key is present. This is what makes the sqlite cross-pod/shared-cache regression test (§8) meaningful and keeps prod/staging (Postgres) and shared-sqlite on the same code path.
- **File store** only when there is **no** SQL store (memory), or explicitly forced for genuine single-process local use.
- Log the chosen store (`store_backed` vs `file` vs `none`) at startup so a misselection is obvious.

### 5.2 Concurrency & token refresh

- The API pod (watchSession) promotes while the worker restores/promotes — Postgres UPSERT with `updated_at` gives last-write-wins; both write the same logical token, so races are benign.
- Codex refreshes the OpenAI/ChatGPT access token during long sessions and rewrites `auth.json`. `promoteCodexAuth` already runs on the success path (`codex.go:290`) — with the shared store this refreshed token is written back to the DB and picked up by the next restore on any pod. Confirm promote is invoked after a refresh, not only at initial connect (open question O3).

## 6. Migration & rollout

- **Schema:** additive. Add the table to **both** the raw `MigratePostgres` (`CREATE TABLE IF NOT EXISTS`) **and** GORM `AutoMigrate` (sqlite/local), per the `runtime_message_id` incident (`2a6400e`). Both run inside `openStore`'s SQL branch (`cmd/agent-runtime/main.go:182-186`, worker `:132-136`), so the table is created before the auth store is used. No `ALTER` needed (new table), but the create must exist on both paths.
- **Backward compatible:** existing file-store tokens are pod-local and ephemeral — there is nothing worth migrating; on first run with the new store users complete one sign-in that now persists. `api_key`-mode and non-codex runs are unaffected.
- **Enablement:** ship the code; on staging it activates automatically (DB + key present). `AGENT_RUNTIME_CODEX_AUTH_DIR` becomes unused for the DB path (keep it read for the local fallback).
- **Rollback:** revert to the file store by config; behavior returns to the (broken) pod-local state — so rollback is safe but re-exposes the loop.

## 7. Edge cases

- **Scope consistency:** promote (API) and restore (worker) must compute an identical `CodexAuthScope`. Verify `codexAuthScope(execCtx, provider, authMode)` yields the same `AppID`/`TenantID` on both paths for the same run/workspace (it should — same execCtx-derived app/tenant). Add a test.
- **Token refresh mid-run** (see §5.2 / O3).
- **Logout / auth failure:** `Clear` must delete the DB row so the next run re-prompts cleanly.
- **Encryption key rotation:** a rotated key cannot decrypt existing rows → treat decrypt failure as "no token" (re-prompt) rather than a hard error; document that rotation forces re-auth.
- **Multiple concurrent runs, same scope:** all share one token row — fine; the first sign-in serves all.
- **Multi-tenant:** the `tenant_id` in the PK keeps tenants' tokens separate; confirm `TenantID` is populated where multi-tenant isolation is required (O2).

## 8. Testing

- **Unit** (store-backed store): promote writes an encrypted row; restore reproduces the exact `auth.json`; restore with no row is a no-op; clear deletes; decrypt failure (wrong key) → treated as no token.
- **Cross-pod simulation:** promote via one store instance, restore via a **second** instance sharing the same DB (memory/sqlite shared cache) → token round-trips. This is the regression test for the loop.
- **Scope match:** the scope computed on the manager path equals the adapter path for the same run.
- **Migration parity:** table exists after `MigratePostgres` and after `AutoMigrate`.
- **Manual (staging):** enable, complete one ChatGPT device sign-in, confirm the run proceeds and a **second** codex run (and a worker restart) needs **no** re-auth.

## 9. Risks & mitigations

- **Plaintext tokens in DB** → encryption mandatory; refuse to construct the store without a 32-byte key; tokens are AES-256-GCM at rest.
- **Wrong selection (file store still used on staging)** → log the chosen store at startup; assert store-backed when DB+key present; the cross-pod test guards the behavior.
- **Refresh not re-promoted** → verify/instrument the promote-on-refresh path (O3); otherwise a refreshed token stays pod-local and the loop can recur after the access token rotates.
- **Migration only on one path** → add to both `MigratePostgres` and `AutoMigrate` (hard requirement).

## 10. Alternatives considered

- **RWX shared volume** mounted at the auth dir in both pods: simplest conceptually, but `hcloud-volumes` is **RWO** (single-node block). Would need an NFS/RWX provisioner or pinning both pods to one node — fragile and infra-heavy. Rejected as the primary fix.
- **k8s Secret-backed store:** shared and durable, but awkward (RBAC, per-write API churn, secret-size limits, audit noise). Rejected in favor of the DB the runtime already owns.
- **Co-locate API + worker in one pod:** breaks the deployment model and doesn't scale. Rejected.

## 11. Open questions

1. O1 — *(resolved, see §5)* standalone `codexAuthRepository` over `*gorm.DB` (via `(*store.SQL).DB()`), not a widened `agentcore.Store`.
2. O2 — Is `TenantID` populated for helpin/usermaven scopes today? If always empty, the PK degrades to `app_id+provider+auth_mode` (one token per app) — confirm that is the intended isolation.
3. O3 — Does `promoteCodexAuth` run after codex refreshes the token mid-session, or only at initial connect? If only at connect, add a promote-after-refresh hook so rotated tokens persist.
4. O4 — Should `Clear` also fire on repeated `account/read` "requires auth" to self-heal a corrupt/stale row?

## 12. Code references

- Interface + scope + file store: `internal/runtime/codex_auth.go` (`CodexAuthStore`, `CodexAuthScope`, `FileCodexAuthStore`, `scopePath`).
- Store construction: `internal/runtime/codex.go:48` (`DefaultCodexConfigFromEnv`), `:60` (`NewEncryptedFileCodexAuthStore`), `:311-331` (restore/promote/clear wrappers).
- Device-code manager (promote on completion): `internal/runtime/codex_auth_manager.go` (`StartDeviceCode`, `watchSession`, `NewCodexAuthManager`).
- Adapter auth preflight: `internal/runtime/codex.go:271` (`ensureCodexAuthenticated`), `:290` (`promoteCodexAuth`).
- Manager wiring with DB handle: `cmd/agent-runtime/main.go:103`.
- Migration parity precedent: commit `2a6400e` (runtime_message_id).
