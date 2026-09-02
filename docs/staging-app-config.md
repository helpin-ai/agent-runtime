# Staging App Config (`AGENT_RUNTIME_APP_CONFIG`)

Canonical staging value for the `AGENT_RUNTIME_APP_CONFIG` secret. Staging is
deployed from `k8s/stage/` via ArgoCD; all runtime env comes from the
`agent-runtime-secrets` Kubernetes secret, which is populated by the
ExternalSecret in `k8s/stage/secrets.yaml` from the Doppler project behind the
`doppler-agent-runtime-api` ClusterSecretStore (`dataFrom: find` — every Doppler
secret in that config becomes an env var).

**This file is documentation only.** The runtime does not read it. The JSON
below is the value to paste into Doppler as the `AGENT_RUNTIME_APP_CONFIG`
secret (single-line or multi-line JSON both work; the runtime parses the raw
string).

## Rules

- `usermaven` is a live production consumer of this runtime. Its app entry in
  the current Doppler value MUST be copied over verbatim and left untouched.
  The placeholder object below only marks its position — never paste the
  placeholder itself.
- Never commit literal secrets here. `<HELPIN_INTERNAL_API_SECRET>` is a
  placeholder for Helpin's `INTERNAL_API_SECRET` (staging value, from the
  Helpin Doppler config). Substitute it when composing the Doppler value, or
  add a `HELPIN_INTERNAL_API_SECRET` secret to this Doppler config and use a
  Doppler secret reference (`${HELPIN_INTERNAL_API_SECRET}`) inside the JSON
  value so the literal appears only once.
- All `https://stage.helpin.ai/api/internal/agent-runtime/...` endpoints are
  Helpin host-adapter endpoints protected by bearer auth against Helpin's
  `INTERNAL_API_SECRET`. Each `token` / `context_token` below must therefore
  carry that secret.

## Merged staging value

```json
{
  "apps": [
    {
      "app_id": "usermaven",
      "_do_not_edit": "PLACEHOLDER — replace this whole object with the existing usermaven entry copied verbatim from the current Doppler AGENT_RUNTIME_APP_CONFIG value."
    },
    {
      "app_id": "helpin",
      "event_protocol": "v2",
      "context_endpoint": "https://stage.helpin.ai/api/internal/agent-runtime/target-context",
      "context_token": "<HELPIN_INTERNAL_API_SECRET>",
      "mcp_providers": [{
        "name": "helpin",
        "transport": "http",
        "url": "https://stage.helpin.ai/api/internal/agent-runtime/mcp/helpin",
        "token": "<HELPIN_INTERNAL_API_SECRET>",
        "tool_namespace": "none",
        "refresh_interval": "30s",
        "startup_policy": "required",
        "unknown_refresh_cooldown": "30s"
      }],
      "skill_provider": {
        "transport": "http",
        "base_url": "https://stage.helpin.ai/api/internal/agent-runtime/skills",
        "package_base_url": "https://stage.helpin.ai/api/internal/agent-runtime/skill-packages",
        "token": "<HELPIN_INTERNAL_API_SECRET>"
      },
      "workspace_provider": {
        "transport": "repository",
        "base_url": "https://stage.helpin.ai/api/internal/agent-runtime/workspace",
        "token": "<HELPIN_INTERNAL_API_SECRET>",
        "root_dir": "/var/lib/agent-runtime-workspaces"
      },
      "browser": {
        "enabled": true,
        "allowed_domains": ["*"],
        "artifact_provider": {
          "transport": "http",
          "upload_endpoint": "https://stage.helpin.ai/api/internal/agent-runtime/artifacts",
          "token": "<HELPIN_INTERNAL_API_SECRET>"
        }
      }
    }
  ]
}
```

## Endpoint derivation (do not change paths)

The runtime appends fixed suffixes to each base URL
(`internal/appconfig/config.go`, `internal/skills/http_lookup.go`, `internal/skills/http_package_store.go`,
`internal/workspace/http_provider.go`):

| Config field | Runtime calls | Helpin route |
| --- | --- | --- |
| `context_endpoint` | `POST` as-is | `POST /api/internal/agent-runtime/target-context` |
| `mcp_providers[].url` | `GET {url}/tools`, `POST {url}/call` | `GET /api/internal/agent-runtime/mcp/helpin/tools`, `POST /api/internal/agent-runtime/mcp/helpin/call` |
| `skill_provider.base_url` | `POST {base}/by-id`, `POST {base}/active-by-key` | `POST /api/internal/agent-runtime/skills/by-id`, `/skills/active-by-key` |
| `skill_provider.package_base_url` | `GET {base}/objects/{key}` | `GET /api/internal/agent-runtime/skill-packages/objects/*` |
| `workspace_provider.base_url` (repository mode) | `POST {base}/repository-spec` | `POST /api/internal/agent-runtime/workspace/repository-spec` |
| `browser.artifact_provider.upload_endpoint` | `POST` as-is | `POST /api/internal/agent-runtime/artifacts` |

`workspace_provider.root_dir` is a path inside the runtime worker pod where
repository workspaces are prepared. Staging worker pods currently have a
writable root filesystem and no dedicated volume, so workspaces are ephemeral
per pod; add an `emptyDir` mount at `/var/lib/agent-runtime-workspaces` in
`k8s/stage/worker-deployment.yaml` if the pod is later hardened with
`readOnlyRootFilesystem`.

## Related staging secrets (same Doppler config)

- `AGENT_RUNTIME_SERVICE_TOKEN` — REQUIRED. The runtime fails closed at
  startup without it (unless `AGENT_RUNTIME_ALLOW_ANONYMOUS=true`, which must
  never be set in staging/prod). Helpin's staging config must carry the same
  value in its own `AGENT_RUNTIME_SERVICE_TOKEN` secret.
- `AGENT_RUNTIME_EVENT_SINK=log,nats` and `AGENT_RUNTIME_NATS_URL` — must point
  at the SAME NATS JetStream cluster as Helpin's `NATS_URL`; the Helpin
  temporal-worker consumes the `AGENT_RUNTIME_EVENTS` stream for run
  projection. Apps that need HTTP delivery should define `event_callbacks`
  inside their own app entry rather than adding the legacy global `callback`
  sink.

Full ops runbook (rollout order, verification, rollback):
`helpin` repo → `docs/AGENT_RUNTIME_STAGING.md`.
