# Host app configuration

`AGENT_RUNTIME_APP_CONFIG` connects each product to its target context, domain
commands, skills, MCP servers, and workspace provider. JSON remains supported;
YAML files are easier to review and can reference token environment variables.

```bash
AGENT_RUNTIME_APP_CONFIG=@/etc/agent-runtime/apps.yaml
HELPIN_INTERNAL_API_SECRET=...
USERMAVEN_INTERNAL_API_SECRET=...
```

```yaml
apps:
  - app_id: helpin
    event_protocol: v2
    context_endpoint: https://stage.helpin.ai/api/internal/agent-runtime/target-context
    context_token_env: HELPIN_INTERNAL_API_SECRET
    command_provider:
      transport: http
      base_url: https://stage.helpin.ai/api/internal/agent-runtime/commands
      token_env: HELPIN_INTERNAL_API_SECRET
    skill_provider:
      transport: http
      base_url: https://stage.helpin.ai/api/internal/agent-runtime/skills
      package_base_url: https://stage.helpin.ai/api/internal/agent-runtime/skill-packages
      token_env: HELPIN_INTERNAL_API_SECRET
    workspace_provider:
      transport: repository
      base_url: https://stage.helpin.ai/api/internal/agent-runtime/workspace
      token_env: HELPIN_INTERNAL_API_SECRET
      root_dir: /var/lib/agent-runtime-workspaces
  - app_id: usermaven
    event_callbacks:
      - url: http://usermaven-server-svc.default.svc.cluster.local/agent-runtime/events
        token_env: USERMAVEN_INTERNAL_API_SECRET
        event_types:
          - run.completed
          - run.failed
          - run.cancelled
          - run.paused
```

Only `app_id` is always required. Provider blocks are optional:

| Block | Use it when |
| --- | --- |
| `context_endpoint` | Targets need product-owned context beyond their ID. |
| `command_provider` | Agents call product-domain tools such as task or document commands. |
| `skill_provider` | Skills are stored or versioned by the host product. |
| `workspace_provider` | Runs need host-authorized repository checkout and delivery. |
| `mcp_providers` | The app supplies additional MCP tools. |
| `event_callbacks` | The app needs selected runtime events delivered to an HTTP endpoint. |

`event_protocol` is app-scoped. Omit it (or set `v1`) for the existing event
contract. Set `v2` only for consumers that use durable per-run sequence numbers,
stable segment identities, and the `/v2/runs/{run_id}/events` replay API. This
allows Helpin to opt into v2 without changing Usermaven's v1 subjects or callback
payloads.

Tokens may still be placed directly in JSON for backward compatibility. Prefer
`context_token_env`, `token_env`, and `package_token_env` so configuration can
be reviewed without copying secrets into a nested JSON value.

Every provider block is validated for a supported transport and a usable URL.
MCP HTTP providers also accept `token_env`; stdio providers require `command`
instead of `url`.

`mcp_providers` are deployment-owned, static providers loaded into every
matching app run. Workspace/user-selected MCP installation, browser OAuth, and
refresh-token storage belong in the host app instead. Attach the selected
server, exact tools, and a short-lived run credential through
`StartRunRequest.mcp_servers`; see [run-scoped MCP servers](run-scoped-mcp.md).

## Per-app event callbacks

HTTP callbacks are scoped by `app_id`. An event is delivered only to callbacks
inside the matching app entry, and only when its type appears in `event_types`.
Omit `event_types` to deliver every event for that app. Omit
`event_callbacks`, or set it to an empty list, to disable HTTP callbacks for an
app without affecting shared log/NATS delivery.

`token` may be specified inline for backward-compatible secret JSON, but
`token_env` is preferred. The referenced secret must be available to every API
or Temporal worker process that emits events. Each configured callback is
retried independently using the runtime's bounded HTTP callback policy.
Delivery is at least once; callback receivers must handle duplicate events
idempotently.

Per-app callbacks are independent of `AGENT_RUNTIME_EVENT_SINK`; keep that
setting focused on shared sinks, for example:

```bash
AGENT_RUNTIME_EVENT_SINK=log,nats
```

The legacy global `AGENT_RUNTIME_EVENT_CALLBACK_URL` and
`AGENT_RUNTIME_EVENT_CALLBACK_TOKEN` remain supported when the global sink
list includes `callback`. Do not combine a global callback with a matching
per-app callback unless duplicate delivery is intentional. Shared deployments
should use per-app callbacks.

Validate before deploying:

```bash
go run ./cmd/agent-runtime-config validate /etc/agent-runtime/apps.yaml
```

Probe the configured endpoints without exposing their tokens:

```bash
go run ./cmd/agent-runtime-config doctor /etc/agent-runtime/apps.yaml helpin
```

The console Configuration page shows the same sanitized app topology and the
latest connectivity result. Tool definitions and handlers are app-scoped; two
products may use the same tool alias without overwriting or exposing one
another's command endpoint.
