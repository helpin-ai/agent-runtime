# Deploy Agent Runtime

This guide describes Helpin’s staging and production deployment of the runtime.
It is an operator reference for that infrastructure, not a requirement for
integrating another application. Start with the [quickstart](quickstart.md) for
a local evaluation and [runtime configuration](runtime-configuration.md) for
worker images and environment variables.


Agent Runtime is deployed to two Kubernetes clusters (staging and production) via
**ArgoCD GitOps**. The private `helpin-ai/gitops` repository holds the Kubernetes
manifests. The ArgoCD `agent-runtime` Applications in each cluster track its
`main` branch at `agent-runtime/stage` or `agent-runtime/prod`.

### Environments and branch flow

| Branch | Environment | Image tag | Manifests |
| --- | --- | --- | --- |
| `develop` | staging | `vX.Y.Z-rc.N` / `stage-latest` | `helpin-ai/gitops:agent-runtime/stage/` |
| `main` | production | `vX.Y.Z` / `prod-latest` | `helpin-ai/gitops:agent-runtime/prod/` |

- **`ci.yml`** (PRs + pushes): Go/React tests, runtime image toolchain smoke
  checks, and container builds. The GitOps repository validates both overlays.
- **`staging-release.yml`** (push to `develop`): builds + pushes the image to
  `ghcr.io/helpin-ai/agent-runtime`, computes an RC version, and commits the new
  tag into `helpin-ai/gitops:agent-runtime/stage/kustomization.yaml`.
- **`production-release.yml`** (push to `main`): same for a stable `vX.Y.Z` tag,
  bumping `helpin-ai/gitops:agent-runtime/prod/kustomization.yaml` and cutting a GitHub release.

ArgoCD (both environments track `helpin-ai/gitops:main`) syncs the bumped manifests
automatically. The API remains an internal `ClusterIP` service
`agent-runtime:8090`; a separate worker Deployment runs the durable Temporal
worker. The operator console is built as `agent-runtime-console`, talks to that
internal service through its server-side BFF, and is exposed through a
TLS/basic-auth protected ingress. See
[`packages/console/README.md`](../packages/console/README.md) for certificate and
credential prerequisites.

Runtime image changes are promoted through staging before production. After an
RC reaches staging, verify support workflows and the opt-in native coding
smokes before enabling coding presets. CI prints the
uncompressed image size so large toolchain regressions are visible during
review.

### Postgres

Postgres is **not** embedded in these manifests. It is a separate ArgoCD
Application (`agent-runtime-pg`) that deploys the
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
- `AGENT_RUNTIME_PYTHON_ENV_TIMEOUT` bounds private Python environment
  initialization (default `5m`; duration strings or positive seconds).
  Environments are pip-free; `pip`, `pip3`, and `python -m pip` use the image's
  package tooling while targeting the private venv, with cache and bytecode
  writes disabled to reduce RWX metadata traffic.
- `AGENT_RUNTIME_EPHEMERAL_ROOT` moves regenerable per-run toolchain state
  (Python venv, temporary files, package caches, Go caches, and Cargo target)
  off the workspace volume. Kubernetes coding workers mount a 20Gi `emptyDir`
  at `/tmp/agent-runtime-ephemeral`; a retry on another pod rebuilds this state.
  The path must be absolute and must not be a symlink or the filesystem root.
- `AGENT_RUNTIME_WORKER_HEALTH_ADDR` controls the worker liveness/readiness
  listener (default `:8091`). `/healthz` is process-only; `/readyz` stays
  unavailable until every required tool provider has a usable catalog.
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
- **ArgoCD needs read access** to `helpin-ai/gitops` via a dedicated read-only GitHub
  **deploy key**, stored as an ArgoCD `repository` secret in each cluster.
- The Doppler service token must exist in-cluster as
  `doppler-token-agent-runtime-api` in the `agent-runtime` namespace.

### Agent registration (host app responsibility)

Agent Runtime is host-neutral and **never creates its own agents** — the host
application registers app-scoped agents via `POST /v1/agents` or `PUT /v1/agents/{id}`. This is a host-app
step (e.g. running its agent-sync helper) and is **not** automatic on deploy; it
must be re-run after changing the host's agent definitions or resetting the
Agent Runtime database, in each environment. A missing agent surfaces as
`502 {"detail":"agent not found"}` from the host on run creation.
