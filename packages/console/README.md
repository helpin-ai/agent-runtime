# Agent Runtime Console

Standalone operator UI for the Agent Runtime service. Built with
**TanStack Start** (React 19 + Vite), **TanStack Query**, **Tailwind v4**, and
**shadcn/ui**.

## Architecture

The console never talks to the Go runtime from the browser. Every call goes
through a **server function** (`src/lib/runtime.server.ts`), which runs only on
the server and injects the bearer token from the environment. The browser calls
those functions; the token and the runtime's address stay server-side.

```
browser ── TanStack Query ──▶ server fn (createServerFn) ──▶ runtime-api.ts ──▶ Go runtime /v1
                                  (token injected here)
```

- `src/lib/types.ts` — models mirrored from `docs/openapi.yaml`.
- `src/lib/runtime-api.ts` — **server-only** fetch layer (holds the token).
- `src/lib/runtime.server.ts` — `createServerFn` wrappers = the BFF boundary.
- `src/lib/queries.ts` — TanStack Query `queryOptions` (incl. run polling).
- `src/lib/health.ts` — derives "needs attention" items (paused/failed runs,
  agents whose model provider has no key configured).
- `src/routes/` — file-based routes: dashboard, agents (+ detail), runs
  (+ detail), and **config**.

## Views

- **Dashboard** (`/`) — the four headline areas: run activity counts, a
  configuration summary, recent runs, and a "needs attention" panel.
- **Configuration** (`/config`) — full read-only picture of what the runtime
  has wired: model providers (configured? default model), store driver, durable
  /Temporal, service auth, tools (by category) and skills, plus how this console
  is connected. Backed by `GET /capabilities` on the runtime.
- **Agents** (`/agents`, `/agents/$id`) — list + full per-agent configuration
  (runtime, provider/model, approval mode, allowed tools/targets, system prompt,
  execution config), with a warning if the agent's provider isn't configured.
  Create at `/agents/new` and edit at `/agents/$id/edit` (shared `AgentForm`;
  tool selection is driven by `/capabilities`).
- **Runs** (`/runs`, `/runs/$id`) — searchable, status-filtered, paginated list
  + transcript / tool-calls / interactions / artifacts, with approve / cancel /
  request-changes. Launch a run at `/runs/new`. The detail view streams **live**
  over SSE (see below).

## Live updates (SSE)

The runtime exposes `GET /runs/{id}/events` (`text/event-stream`), fed by an
in-process broker that receives every engine + adapter event
(`assistant_message_*`, `tool_call_*`, `run.*`). The browser's `EventSource`
hits the **same-origin** proxy route `src/routes/api.runs.$runId.events.ts`,
which pipes the runtime stream through with the service token injected
server-side. `src/lib/use-run-stream.ts` consumes it: it shows in-flight
assistant text live and invalidates the run-detail query on state-changing
events. Polling is reduced to a 15s fallback for a dropped stream.

> The `GET /capabilities` endpoint was added to the Go runtime
> (`internal/api/capabilities.go`) to expose this non-secret config; it never
> returns API keys, only whether each provider is configured.

## Setup

```bash
cp .env.example .env   # point at your runtime; set token + /v1 for non-local
npm install
npm run dev            # http://localhost:3000
```

Make sure the Go runtime is running (`go run ./cmd/agent-runtime`, listens on
`:8090`).

## Serving (production)

`npm run dev` is for local development only — it ships an unminified module
graph, an HMR socket, and mounts the Router/Query devtools. **Do not put the
dev server behind a public domain**; browsers will flag it as slow. Serve a
production build instead:

```bash
npm run build                              # → .output/ (minified, no devtools)
PORT=3100 HOST=127.0.0.1 \
  AGENT_RUNTIME_BASE_URL=http://localhost:8090 \
  AGENT_RUNTIME_API_PREFIX=/v1 \
  AGENT_RUNTIME_APP_ID=usermaven \
  node .output/server/index.mjs
```

The Nitro server reads `AGENT_RUNTIME_*` from the runtime environment (it does
**not** auto-load `.env`), so pass them on launch (or via your service manager).
Devtools are gated on `import.meta.env.DEV`, so they never ship in the build.

### Run as a service (systemd)

A unit is provided at `deploy/agent-runtime-console.service` (loads `.env` via
`EnvironmentFile`, serves the prebuilt `.output` on `127.0.0.1:3100`):

```bash
sudo cp deploy/agent-runtime-console.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now agent-runtime-console
# after code changes:
npm run build && sudo systemctl restart agent-runtime-console
```

## Configuration

| Env var | Default | Purpose |
| --- | --- | --- |
| `AGENT_RUNTIME_BASE_URL` | `http://localhost:8090` | Runtime address |
| `AGENT_RUNTIME_API_PREFIX` | `/v1` | Runtime API prefix |
| `AGENT_RUNTIME_SERVICE_TOKEN` | _(empty)_ | Bearer token for the service API |
| `AGENT_RUNTIME_APP_ID` | `host_app` | App scope for all calls |

## Adding shadcn components

Core primitives (button, card, badge, table) are vendored. Add more with:

```bash
npx shadcn@latest add dialog dropdown-menu tabs sonner
```

## Generating API types (optional)

To replace the hand-written `src/lib/types.ts` with a generated source of truth:

```bash
npx openapi-typescript ../../docs/openapi.yaml -o src/lib/api.gen.ts
```

## Follow-ups

- **SSE / live transcript** — runs currently poll every `RUN_POLL_MS` (3s). When
  the runtime exposes a stream endpoint for run messages, consume it via
  `EventSource` (or proxy through a server route to keep the token server-side)
  and drop the polling interval.
- Agent create/edit forms (the `createAgent` / `startRun` server fns are ready).
