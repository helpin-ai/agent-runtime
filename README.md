# Agent Runtime

Host-neutral execution engine for host-owned AI agents.

This repository is intentionally host-neutral. Host applications plug in by
registering app-scoped agents, targets, context providers, MCP tools, workspace
providers, and optional tool packs.

## What is implemented

- App-scoped agent and run models.
- Generic target contract.
- In-memory store for local development and tests.
- GORM SQL store for Postgres/sqlite with JSON/null-byte
  sanitization.
- Lightweight non-Temporal executor.
- Temporal durable executor, workflow, activities, and worker command.
- Native SDK runtime adapter with Eino/Anthropic model execution and built-in
  human input/approval interaction tools, plus a Codex runtime adapter with
  command-wrapper and app-server protocol paths.
- Tool registry with built-in workspace filesystem/search/command/patch/git
  tools, shared host-command-backed tool metadata, MCP provider registration,
  MCP gateway, and stdio MCP bridge command.
- Host app adapter registry for app-specific target context and tool packs.
- Internal HTTP API for agents, runs, messages, artifacts, interactions, and
  run controls.
- Native React package for embedding run transcript/artifact/interaction UI.
- Optional host-prepared or runtime-prepared repository workspaces for writable
  repository execution.

## Interface docs

See [docs/interfaces.md](docs/interfaces.md) for integration contracts and
[docs/openapi.yaml](docs/openapi.yaml) for the versioned HTTP API. See
[docs/agent-ownership.md](docs/agent-ownership.md) for the boundary between a
host application's system/custom agent concepts and Agent Runtime's generic
executor. See
[docs/repository-workspaces.md](docs/repository-workspaces.md) for repository
workspace integration and [docs/app-configuration.md](docs/app-configuration.md)
for the multi-product host configuration format and diagnostics. See
[docs/run-scoped-mcp.md](docs/run-scoped-mcp.md) for app-owned workspace MCP,
OAuth, credential, and per-run tool configuration. Public SDKs live in separate repositories:
`github.com/helpin-ai/agent-runtime-go` and
`github.com/helpin-ai/agent-runtime-python`.

## Run

```bash
go test ./...
go run ./cmd/agent-runtime
```

The service listens on `:8090` by default.

Durable worker:

```bash
TEMPORAL_ADDRESS=localhost:7233 go run ./cmd/agent-runtime-worker
```

### Runtime image toolchain

The production runtime image includes the non-root execution binaries plus the
general-purpose repository toolchain used by Codex, OpenCode, and native SDK
runs: Git, curl, ripgrep, Make and native build tools, Go 1.24.3, Node/npm,
pnpm, Yarn, Python/pip, pytest, uv, Poetry, Rust/Cargo, Codex, and OpenCode.
Language/runtime base versions and npm/Python CLIs are pinned in `Dockerfile`;
Debian packages continue to receive Bookworm security updates. Apt, npm, pip,
and build caches are removed from the final layer.

Semgrep, Trivy, Gitleaks, their databases, and scanner rules are intentionally
not part of this image. The runtime scanner tools continue to report an
unavailable scanner when those executables are not supplied separately.

Build and verify the image under the same non-root user with a hardened
read-only root filesystem:

```bash
docker build -t agent-runtime:local .
bash scripts/container-toolchain-smoke.sh agent-runtime:local
```

React package:

```bash
cd packages/react
npm test
```

Key environment variables:

- `AGENT_RUNTIME_STORE_DRIVER`: `memory`, `sqlite`, or `postgres`
- `DATABASE_URL`: Postgres DSN when using Postgres
- `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`, `PGSSLMODE`:
  used to build the Postgres DSN when `AGENT_RUNTIME_STORE_DRIVER=postgres`
  and `DATABASE_URL` is not set
- `AGENT_RUNTIME_SERVICE_TOKEN`: required bearer token for `/v1` and legacy
  `/internal` service APIs
- `AGENT_RUNTIME_ALLOW_ANONYMOUS`: local-development escape hatch. Set to
  `true` only for isolated local runs without `AGENT_RUNTIME_SERVICE_TOKEN`.
- `AGENT_RUNTIME_APP_CONFIG`: JSON app adapter/MCP config, or `@/path/file.json`
  (also supports app-scoped, event-type-filtered HTTP callbacks)
- `AGENT_RUNTIME_MCP_CREDENTIAL_ENCRYPTION_KEY`: shared API/worker key used to
  encrypt credentials attached to individual runs; required when credentials
  are supplied
- `AGENT_RUNTIME_MCP_ALLOWED_HOSTS`: optional comma-separated host allowlist
  for run-scoped MCP URLs
- `AGENT_RUNTIME_MCP_ALLOW_PRIVATE_NETWORKS`: opt in to trusted private-network
  MCP destinations; denied by default
- `AGENT_RUNTIME_MCP_ALLOW_HTTP`: local-development-only HTTP opt-in for
  run-scoped MCP; HTTPS is required by default
- `ANTHROPIC_API_KEY`: enables Eino-backed Anthropic `native_sdk` execution
- `OPENAI_API_KEY`: enables Eino-backed OpenAI Responses `native_sdk` execution
- `OPENROUTER_API_KEY`: enables Eino-backed OpenRouter Responses `native_sdk` execution
- `EXA_API_KEY`: registers the default `web_search_exa` tool in both the API
  and durable worker processes. Enabling the tool on an agent grants permission
  but does not supply this provider credential.
- `BRAVE_SEARCH_API_KEY` or `BRAVE_API_KEY`: registers the default
  `web_search_brave` tool
- `WEB_FETCH_PROXY_URLS`: optional comma/newline-separated proxy URLs for
  `fetch_url` and `crawl_url`
- `AGENT_RUNTIME_BROWSER_ENABLED`: enables shared Kernel browser infrastructure
  when set to `true`; `KERNEL_API_KEY` is also required. Each host app must opt
  in through its `AGENT_RUNTIME_APP_CONFIG` `browser` block.
- `AGENT_RUNTIME_BROWSER_SESSION_TIMEOUT_SECONDS`: Kernel session timeout,
  default `300`; the runtime closes sessions when runs become terminal and the
  idle timeout cleans up sessions retained across paused conversation turns
- `AGENT_RUNTIME_BROWSER_MAX_OUTPUT_CHARS`: maximum compact snapshot output,
  default `8000`
- `AGENT_RUNTIME_BROWSER_REPLAY_FRAMERATE`: Kernel replay frame rate used by
  `browser_record`, default `15` and bounded to `1`-`20`
- `AGENT_RUNTIME_BROWSER_RECORDING_SMART_TRIM_ENABLED`: automatically removes
  idle agent-reasoning gaps from recordings while retaining browser actions
  and page loads; defaults to `true`
- `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_TIMEOUT_SECONDS`: FFmpeg processing
  timeout, default `90` and bounded to `10`-`300`
- `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_THREADS`: FFmpeg video encoder threads
  per recording, default `1` and bounded to `1`-`4`
- `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_MAX_CONCURRENT`: maximum simultaneous
  FFmpeg jobs per Agent Runtime process, default `1` and bounded to `1`-`4`
- `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_PRE_PADDING_MS` and
  `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_POST_PADDING_MS`: context retained
  around each browser operation, default `750` and `1500` milliseconds
- `AGENT_RUNTIME_BROWSER_FFMPEG_BINARY`: FFmpeg binary override; the runtime
  image includes `ffmpeg` and uses it by default
- `KERNEL_BASE_URL`: optional Kernel API base URL override for self-hosted or
  development environments

Browser domains and private artifact upload credentials are app-scoped under
`AGENT_RUNTIME_APP_CONFIG`; see `docs/app-configuration.md`. Browser sessions
are created through the Kernel API, attached to agent-browser over CDP, and
deleted at run cleanup. They are ephemeral and do not use Kernel profiles.
- `AGENT_RUNTIME_NATIVE_MODEL`: optional native SDK model override; defaults
  by provider are `claude-opus-4-8` for Anthropic, `gpt-5.6-terra` for OpenAI,
  and `openai/gpt-5.6-terra` for OpenRouter
- Codex and OpenCode use the same provider-specific defaults when an agent does
  not supply a model. Codex defaults to OpenAI; OpenCode and the native SDK keep
  their existing Anthropic provider default when no provider is supplied.
- `TEMPORAL_ADDRESS`: enables durable Temporal execution
- `TEMPORAL_NAMESPACE`: Temporal namespace, defaults to `default`

## API examples

Create an agent:

```bash
curl -s -H "Authorization: Bearer $AGENT_RUNTIME_SERVICE_TOKEN" \
  localhost:8090/v1/agents -d '{
  "app_id": "host_app",
  "name": "Target Agent",
  "runtime_kind": "native_sdk",
  "system_prompt": "Help with the target.",
  "allowed_tools": ["get_context"],
  "allowed_targets": ["task"]
}'
```

Start a run:

```bash
curl -s -H "Authorization: Bearer $AGENT_RUNTIME_SERVICE_TOKEN" \
  localhost:8090/v1/runs -d '{
  "app_id": "host_app",
  "agent_id": "agent_id_from_create",
  "target": {"type": "task", "id": "task_123"},
  "instructions": "Summarize this target."
}'
```

## Deployment

Agent Runtime is deployed to two Kubernetes clusters (staging and production) via
**ArgoCD GitOps**. This repo holds the Kubernetes manifests; the org GitOps repos
(`kubernetes-manifests-staging` / `kubernetes-manifests-production`) hold the
ArgoCD `Application` CRDs that point back at these manifests and at the Postgres
Helm chart.

### Environments and branch flow

| Branch | Environment | Image tag | Manifests |
| --- | --- | --- | --- |
| `develop` | staging | `vX.Y.Z-rc.N` / `stage-latest` | `k8s/stage/` |
| `main` | production | `vX.Y.Z` / `prod-latest` | `k8s/prod/` |

- **`ci.yml`** (PRs + pushes): Go/React tests, runtime image toolchain smoke
  checks, container builds, and `kubectl kustomize` of both overlays.
- **`staging-release.yml`** (push to `develop`): builds + pushes the image to
  `ghcr.io/helpin-ai/agent-runtime`, computes an RC version, and commits the new
  tag into `k8s/stage/kustomization.yaml`.
- **`production-release.yml`** (push to `main`): same for a stable `vX.Y.Z` tag,
  bumping `k8s/prod/kustomization.yaml` and cutting a GitHub release.

ArgoCD (staging tracks `develop`, prod tracks `main`) syncs the bumped manifests
automatically. The API remains an internal `ClusterIP` service
`agent-runtime:8090`; a separate worker Deployment runs the durable Temporal
worker. The operator console is built as `agent-runtime-console`, talks to that
internal service through its server-side BFF, and is exposed through a
TLS/basic-auth protected ingress. See
[`packages/console/README.md`](packages/console/README.md) for certificate and
credential prerequisites.

Runtime image changes are promoted through staging before production. After an
RC reaches staging, verify at least one native SDK repository command plus one
Codex and one OpenCode run before merging the release to `main`. CI prints the
uncompressed image size so large toolchain regressions are visible during
review.

### Postgres

Postgres is **not** embedded in these manifests. It is a separate ArgoCD
Application (`agent-runtime-pg`) in the manifest repos that deploys the
[CloudNativePG `cluster` Helm chart](https://cloudnative-pg.github.io/charts).
The primary is reachable at `agent-runtime-pg-rw.agent-runtime.svc.cluster.local:5432`.
Owner credentials come from a secret synced from Doppler (`cluster.initdb.secret`)
so the password is deterministic and matches `DATABASE_URL`.

### Secrets (Doppler + External Secrets Operator)

App config comes from a dedicated Doppler project, synced by ESO via the
`doppler-agent-runtime-api` `ClusterSecretStore` into `agent-runtime-secrets`
(consumed by the Deployments through `envFrom`). Required Doppler keys per env:

- `DATABASE_URL` (e.g. `postgres://agent_runtime:<pw>@agent-runtime-pg-rw.agent-runtime.svc.cluster.local:5432/agent_runtime?sslmode=require`),
  plus `DB_USERNAME` (`agent_runtime`) and `DB_PASSWORD` — the password in
  `DATABASE_URL` **must equal** `DB_PASSWORD` (CloudNativePG uses it for the owner role).
- `AGENT_RUNTIME_SERVICE_TOKEN`, `AGENT_RUNTIME_APP_CONFIG`, and provider keys.
- `AGENT_RUNTIME_MCP_CREDENTIAL_ENCRYPTION_KEY` when any host app attaches MCP
  credentials. Generate one base64-encoded 32-byte value per environment and
  keep the same value available to both API and worker pods. Never commit it.
- `AGENT_RUNTIME_WORKER_STOP_TIMEOUT` controls graceful Temporal worker drain
  time during deploys (default `2m`; duration strings or positive seconds).
  (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `TEMPORAL_*`).

The API process fails closed when `AGENT_RUNTIME_SERVICE_TOKEN` is absent unless
`AGENT_RUNTIME_ALLOW_ANONYMOUS=true` is explicitly set. Staging and production
must not set `AGENT_RUNTIME_ALLOW_ANONYMOUS`.

After changing Doppler secrets, verify the deployed API rejects unauthenticated
service calls before treating the runtime as locked down:

```bash
curl -i https://<runtime-host>/internal/runs?app_id=<app-id>
# expected: HTTP/1.1 401 Unauthorized
```

Non-secret topology (`AGENT_RUNTIME_ADDR=:8090`, `AGENT_RUNTIME_STORE_DRIVER=postgres`)
lives in the Deployment `env:`, not Doppler. ESO does not restart pods on a secret
change — after editing Doppler, `kubectl -n agent-runtime rollout restart
deploy/agent-runtime deploy/agent-runtime-worker`.

### First-time / bootstrap notes

- **ArgoCD Applications are not auto-discovered.** New `argo-applications/*.yaml`
  must be `kubectl apply`'d to the cluster once (there is no app-of-apps).
- **ArgoCD needs read access** to this repo via a dedicated read-only GitHub
  **deploy key**, stored as an ArgoCD `repository` secret in each cluster.
- The Doppler service token must exist in-cluster as
  `doppler-token-agent-runtime-api` in the `agent-runtime` namespace.

### Agent registration (host app responsibility)

Agent Runtime is host-neutral and **never creates its own agents** — the host
application registers app-scoped agents via `PUT /v1/agents`. This is a host-app
step (e.g. running its agent-sync helper) and is **not** automatic on deploy; it
must be re-run after changing the host's agent definitions or resetting the
Agent Runtime database, in each environment. A missing agent surfaces as
`502 {"detail":"agent not found"}` from the host on run creation.
