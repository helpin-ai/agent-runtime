# Host app configuration

`AGENT_RUNTIME_APP_CONFIG` connects each product to its target context, skills,
MCP servers, and workspace provider. JSON remains supported;
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
    mcp_providers:
      - name: helpin
        transport: http
        url: https://stage.helpin.ai/api/internal/agent-runtime/mcp/helpin
        token_env: HELPIN_INTERNAL_API_SECRET
        tool_namespace: none
        refresh_interval: 30s
        startup_policy: required
        unknown_refresh_cooldown: 30s
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
    browser:
      enabled: true
      allowed_domains: ["*"]
      artifact_provider:
        transport: http
        upload_endpoint: https://stage.helpin.ai/api/internal/agent-runtime/artifacts
        token_env: HELPIN_INTERNAL_API_SECRET
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
| `skill_provider` | Skills are stored or versioned by the host product. |
| `workspace_provider` | Runs need host-authorized repository checkout and delivery. |
| `mcp_providers` | The app supplies additional MCP tools. |
| `event_callbacks` | The app needs selected runtime events delivered to an HTTP endpoint. |
| `browser` | The app opts into shared browser tools and supplies its own domain policy and optional private artifact sink. |

Browser tools are registered per app. `browser_open`, `browser_snapshot`,
`browser_read`, and `browser_act` require `browser.enabled`; `browser_screenshot` additionally
requires `browser.artifact_provider`. Snapshots list interactive elements only, sized for acting on a page;
`browser_read` returns the page's visible text (or one element's, by CSS
selector or snapshot reference) in parts of at most
`AGENT_RUNTIME_BROWSER_MAX_OUTPUT_CHARS`, with `next_offset` to continue.
Browser automation uses local Chromium
when `KERNEL_API_KEY` is absent. When the key is present, Agent Runtime prefers
Kernel but falls back to local Chromium if Kernel reports unavailable billing
credit. Other Kernel errors remain visible instead of silently changing
backends. `browser_record` is registered whenever the artifact provider is
configured. Kernel sessions use native replay recording. Local Chromium
sessions record the existing run session to WebM and Agent Runtime converts the
result to H.264 MP4; local recording rejects `record_audio=true` because that
backend does not capture browser audio. Normal sessions are headless by
default. On a Kernel `browser_record` start, Agent Runtime saves cookies and web
storage into a private temporary file, replaces that run's browser with a
headful Kernel browser, restores the state, and reopens the current URL before
recording. In-memory page state and unsaved form values do not survive this
one-time Kernel promotion. A stop downloads the Kernel MP4 or finalizes the
local WebM inside Agent Runtime. By default, the runtime uses the browser
action and navigation timeline to remove gaps spent on agent reasoning, keeping
750 ms before and 1500 ms after each operation. Nearby windows are merged and
the retained segments are encoded into one H.264 MP4 for documentation use.
Recordings with no removable idle time are uploaded unchanged. FFmpeg failures,
timeouts, invalid output, and size-limit failures also fall back to the original
Kernel MP4 so evidence is not lost. Local WebM conversion failures remain
visible because WebM cannot satisfy the MP4 artifact contract. Only one FFmpeg
job runs per process by default, with one encoder thread and a 90-second timeout; the corresponding
`AGENT_RUNTIME_BROWSER_RECORDING_TRIM_*` environment variables are documented
in [runtime configuration](runtime-configuration.md). The runtime image includes FFmpeg.

The selected MP4 is streamed to the host artifact sink; video bytes, Kernel
session IDs, replay IDs, and provider URLs are not returned to the model.
Recording output includes `smart_trimmed`, `trim_status`, `raw_duration_ms`,
`output_duration_ms`, `trimmed_idle_ms`, and `trim_window_count` so callers can
distinguish concise demos from safe original-file fallbacks. Each agent run
receives an app-isolated, ephemeral browser session. Kernel sessions are created
by Agent Runtime and attached to agent-browser over CDP; local sessions launch
the Chromium included in the default and coding runtime images. Paused
conversation turns retain that session so follow-up actions, screenshots, and
recordings keep the same page and cookies. An active recording is finalized
before the runtime closes a terminal or idle session, with
`AGENT_RUNTIME_BROWSER_SESSION_TIMEOUT_SECONDS` as the idle safety fallback.
Cookies and login state are not persisted after the session closes, and Kernel
browser profiles are not used. Set `AGENT_RUNTIME_BROWSER_CHROMIUM_EXECUTABLE`
only when local browser discovery needs an explicit executable. New sessions
use a `1440x900` viewport by default so documentation screenshots have a
consistent desktop layout without increasing model-facing tool schemas or
requiring per-app environment settings.
`allowed_domains: ["*"]`
means unrestricted browser navigation; Agent Runtime represents that by
omitting agent-browser's domain allowlist rather than forwarding a literal `*`.

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

Provider tool names are prefixed by `tool_prefix`, or by the provider `name`
when no explicit prefix is set. Set `tool_namespace: none` to expose canonical
remote names unchanged; this mode rejects a non-empty `tool_prefix` and any
collision with a runtime-global tool. `refresh_interval` enables atomic catalog
refresh while an omitted value preserves startup-only discovery after the first
successful load. Scheduled and unknown-tool refreshes share one single-flight
path, and retry/refresh delays are jittered so replicas do not synchronize.

`startup_policy: required` keeps API and worker processes alive but unready if
no valid catalog is available. Both retry discovery in the background, and
workers do not poll Temporal queues until discovery succeeds. This preserves
required-provider fail-closed behavior without putting either process into
Kubernetes crash backoff.
`unknown_refresh_cooldown` defaults to 30 seconds and bounds synchronous
refreshes triggered by newly saved tool allowlists.

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

## Optional per-run model credentials

Apps can set `model_credential_callback: {"url": "https://app.example/agent-runtime/model-credentials/refresh", "token_env": "APP_CALLBACK_SECRET"}`. The URL and authentication are trusted deployment configuration; requests cannot override them. Set `AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY` consistently on the API and workers. See [the credential contract and release gates](2026-09-12-run-credentials.md) for app ownership, refresh, and optional ChatGPT enablement.


### Requiring host-supplied model credentials

Set `require_run_model_credentials: true` on an app to require both an explicit
run model and run credential. The default is `false`, preserving standalone and
Usermaven environment-key execution. This is trusted deployment configuration;
it cannot be overridden by run JSON. An explicitly supplied credential never
falls back to an environment key, regardless of this setting.

The API and every worker pass the loaded app policy into the engine. Enforcement
happens in `Engine.StartRun` before queue persistence or dispatch, and on stored-run
resume, repair, and execution. Idempotent retries use the accepted run identity
without requiring the caller to resend secrets. Before enabling this policy for
an existing app, finish or explicitly cancel its old environment-key runs.

Capabilities include `apps[].require_run_model_credentials`, derived from the
loaded app configuration per request. Provider `configured` and
`run_credentials_configured` values remain startup snapshots, not live auth probes.
Restart affected processes after environment-key rotation; changing deployment
secrets alone does not refresh an already running process's capability snapshot.
