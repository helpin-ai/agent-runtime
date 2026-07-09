# Agent Runtime Console

Standalone operator UI for the Agent Runtime service. Built with
**TanStack Start** (React 19 + Vite), **TanStack Query**, **Tailwind v4**, and
**shadcn/ui**.

## Architecture

The console never talks to the Go runtime from the browser. Every call goes
through a **server function** (`src/lib/runtime-fns.ts`), which runs only on
the server and injects the bearer token from the environment. The browser calls
those functions; the token and the runtime's address stay server-side.

```
browser ── TanStack Query ──▶ server fn (createServerFn) ──▶ runtime-api.ts ──▶ Go runtime /v1
                                  (token injected here)
```

- `src/lib/types.ts` — models mirrored from `docs/openapi.yaml`.
- `src/lib/runtime-api.server.ts` — **server-only** fetch layer (holds the token).
- `src/lib/runtime-fns.ts` — `createServerFn` wrappers = the BFF boundary.
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
  /Temporal, app-scoped host adapters and connectivity, service auth, tools (by
  category) and skills, plus how this console is connected. Backed by
  `GET /capabilities` and `GET /app-health` on the runtime.
- **Agents** (`/agents`, `/agents/$id`) — list + full per-agent configuration
  (runtime, provider/model, approval mode, allowed tools/targets, system prompt,
  execution config), with a warning if the agent's provider isn't configured.
  Create at `/agents/new` and edit at `/agents/$id/edit` (shared `AgentForm`;
  tool selection is driven by `/capabilities`).
- **Runs** (`/runs`, `/runs/$id`) — server-filtered, paginated list plus overview,
  persisted timeline, transcript, tool calls, interactions, artifacts, usage,
  workspace, and Temporal execution details. Launch a run at `/runs/new`.

The application selector at the top of the left sidebar is populated from the
runtime's sanitized app catalog. The selected app is stored in the URL as
`?app=<app_id>` and retained across navigation, so links are bookmarkable and
agents, runs, tools, health checks, mutations, and SSE streams always use the
same app scope. `AGENT_RUNTIME_APP_ID` is only the initial default when the URL
does not select a configured app.

## Live updates (SSE)

The runtime exposes `GET /runs/{id}/events` (`text/event-stream`). Events are
persisted by both the API and Temporal worker and replayed by the API, while an
in-process broker wakes local subscribers immediately
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

The console is a privileged operator surface and does not implement end-user
authentication. In any shared environment, keep it on a private network or put
it behind the product's authenticated reverse proxy/SSO. Anyone who can reach
the console can invoke its run and agent operations through the server-side
service credential.

## Container

Build the console as a separate image from the Go runtime. The build context is
the repository root so the committed lockfile is available:

```bash
docker build -f packages/console/Dockerfile \
  -t ghcr.io/helpin-ai/agent-runtime-console:local .
```

The image listens on `0.0.0.0:3000` and exposes `GET /api/health` for Kubernetes
probes. It needs `AGENT_RUNTIME_BASE_URL`, `AGENT_RUNTIME_SERVICE_TOKEN`, and
`AGENT_RUNTIME_APP_ID` at runtime; none of these values are compiled into the
browser bundle.

The checked-in Kubernetes overlays expose:

| Environment | URL | TLS secret |
| --- | --- | --- |
| Staging | `https://agent-runtime.stage.usrmvn.com` | ingress default `*.stage.usrmvn.com` |
| Production | `https://agent-runtime.prod.usrmvn.com` | ingress default `*.prod.usrmvn.com` |

The static overlays intentionally omit `spec.tls[].secretName`, allowing the
`pp-nginx` controller to serve its configured Usermaven wildcard certificate.
This avoids duplicating TLS private keys between the `helpin` and
`agent-runtime` namespaces. The Helm chart still accepts an explicit
`console.ingress.tlsSecretName` for clusters without a suitable default.

The static overlays delegate authentication to the existing GitHub OAuth
proxies at `oauth2-proxy.stage.usrmvn.com` and
`oauth2-proxy.prod.usrmvn.com`. The proxy enforces the organization policy and
returns the authenticated user/email headers to the console ingress; no
console-specific htpasswd secret is required.

Ingress authentication is fail-closed. Add an
`AGENT_RUNTIME_CONSOLE_HTPASSWD` value to the agent-runtime Doppler project;
the overlay's `ExternalSecret` writes it to the NGINX basic-auth `auth` key.
Generate a bcrypt entry with:

```bash
htpasswd -nbB operator '<strong-password>'
```

The Helm chart keeps the console disabled by default. Set `console.enabled`,
`console.ingress.enabled`, `console.appID`, host/TLS values, and either an
existing basic-auth secret or NGINX external-auth annotations to enable it.

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
| `AGENT_RUNTIME_APP_ID` | `host_app` | Initially selected app; operators can switch among configured apps |

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
