# Agent Runtime Interfaces

This project is an internal, host-neutral agent runtime. Host apps plug in through
small interfaces instead of forking the executor.

Agent ownership labels such as system/custom and workspace tenancy are
host-application concepts. Agent Runtime stores and executes the app-scoped
definition it receives; it does not choose or create product agents. See
[Agent Ownership and Execution Boundary](agent-ownership.md).

## Core Data Model

Package: `internal/agentcore`

- `Agent`: app-scoped reusable executor definition.
- `AgentRun`: durable execution primitive for all runtimes.
- `TargetRef`: generic host target `{ type, id, display?, metadata? }`.
- `AgentRunMessage`: ordered transcript entry.
- `AgentRunArtifact`: ordered run artifact.
- `AgentRunInteraction`: pending/resolved human input or approval request.
- `ToolCall`: persisted tool call audit record.
- `WorkspaceLease`: optional opaque lease for host-prepared execution workspaces.

Every durable record is partitioned by `app_id`.

## Store

Package: `internal/agentcore`

```go
type Store interface {
  CreateAgent(ctx context.Context, agent *Agent) error
  GetAgent(ctx context.Context, appID, agentID string) (*Agent, error)
  ListAgents(ctx context.Context, appID string) ([]Agent, error)
  UpdateAgent(ctx context.Context, agent *Agent) error

  CreateRun(ctx context.Context, run *AgentRun) error
  GetRun(ctx context.Context, appID, runID string) (*AgentRun, error)
  ListRuns(ctx context.Context, appID string) ([]AgentRun, error)
  UpdateRun(ctx context.Context, run *AgentRun) error

  AppendMessage(ctx context.Context, message *AgentRunMessage) error
  ListMessages(ctx context.Context, appID, runID string) ([]AgentRunMessage, error)
  AppendArtifact(ctx context.Context, artifact *AgentRunArtifact) error
  ListArtifacts(ctx context.Context, appID, runID string) ([]AgentRunArtifact, error)
  AppendInteraction(ctx context.Context, interaction *AgentRunInteraction) error
  ListInteractions(ctx context.Context, appID, runID string) ([]AgentRunInteraction, error)
  UpdateInteraction(ctx context.Context, interaction *AgentRunInteraction) error
  AppendToolCall(ctx context.Context, call *ToolCall) error
  ListToolCalls(ctx context.Context, appID, runID string) ([]ToolCall, error)
}
```

Implementations:

- `store.NewMemory()` for tests/local dev.
- `store.OpenSQL(...)` for Postgres/sqlite via GORM.

The SQL store includes sanitization for PostgreSQL-hostile null
bytes and invalid JSON.

## Agent Registry API

Apps own agent selection. Agent Runtime stores and executes the selected agent
definition:

- `GET /v1/agents?app_id=...` lists app agents.
- `POST /v1/agents` creates an agent.
- `GET /v1/agents/{agent_id}?app_id=...` reads one agent.
- `PUT /v1/agents/{agent_id}?app_id=...` upserts an agent definition and is
  the preferred bootstrap/sync endpoint for host apps.

`POST /v1/runs` continues to require `agent_id`. Run-level `allowed_tools` can
narrow, but not expand, `Agent.AllowedTools`.

The optional `mcp_servers` field attaches app-selected remote MCP servers only
to that run. Each attachment may carry `skills`; those refs persist with the
attachment and resolve only for that run, so hosts can activate connector
guidance without mutating the saved agent. Required tools are checked against
the saved/run tool policy plus that run's authorized MCP aliases. See
[run-scoped MCP servers](run-scoped-mcp.md) for the wire contract, OAuth
ownership, credential lifecycle, SDK examples, and deployment controls.

Hosts can enforce terminal output contracts through `Agent.ExecutionConfig`:

```json
{"completion":{"required_tools":["publish_task_plan_doc"]}}
```

Before marking a run complete, the engine verifies that every named tool has a
successful recorded call. Paused runs are unaffected, so question and approval
interactions can continue before the terminal check.

## Host App Adapter

Go package: `internal/host`

```go
type AppAdapter interface {
  AppID() string
}

type TargetResolver interface {
  ResolveTarget(ctx context.Context, appID string, target agentcore.TargetRef) (*TargetContext, error)
}

type ToolRegistrar interface {
  RegisterTools(ctx context.Context, registry *tools.Registry) error
}
```

Register adapters through `host.AdapterRegistry`. An adapter can provide target
context, app-specific tools, or both. Unknown apps fall back to the configured
`TargetContextProvider`.

Non-Go apps should use the HTTP target-context adapter instead of importing Go:

```json
{
  "apps": [{
    "app_id": "host_app",
    "context_endpoint": "https://host.internal/agent-runtime/target-context",
    "context_token": "service-token"
  }]
}
```

Set this JSON directly in `AGENT_RUNTIME_APP_CONFIG`, or set
`AGENT_RUNTIME_APP_CONFIG=@/path/to/config.json`.

Target-context request:

```json
{
  "app_id": "host_app",
  "run_id": "run_123",
  "agent_id": "agent_123",
  "target": {"type": "article", "id": "article_123"},
  "trigger": {},
  "metadata": {}
}
```

Target-context response:

```json
{
  "target": {"type": "article", "id": "article_123"},
  "summary": "Current article context for the agent.",
  "data": {}
}
```

## Runtime Adapter

Package: `internal/runtime`

```go
type Adapter interface {
  Kind() string
  Execute(ctx *ExecutionContext) (*Result, error)
}
```

`ExecutionContext` provides the app, agent, run, target context, allowed tools,
optional workspace lease, artifact writer, interaction broker, and event sink.

Implemented adapters:

- `native_sdk`: in-process adapter. Without model configuration it errors
  unless `AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK=true` is set. With
  `ANTHROPIC_API_KEY` or
  `AGENT_RUNTIME_NATIVE_EINO=true`, it uses the Eino-backed native execution
  loop with registered tools.
- `codex`: command-backed adapter when `CODEX_PATH` or explicit config is set;
  without command/app-server configuration it errors unless
  `AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK=true` is set. If a workspace lease
  is present, command-backed Codex runs with `workspace_lease.root_path` as its
  working directory. `CodexConfig.AppServer=true` enables the Codex app-server
  stdio protocol path for `initialize`, `thread/start`, `turn/start`, streaming
  notifications, and approval/input pauses. Agent Runtime automatically uses
  this path when the run has allowed app tools or an active skill requires an
  approval/input interaction, even when `CODEX_APP_SERVER` is not explicitly
  set, because the one-shot command path cannot satisfy those contracts.
  App-server runs persist `codex_session_state` artifacts so paused
  approval/input requests can resume the same Codex thread.
  `CODEX_USE_LEGACY_LANDLOCK=true` swaps the bubblewrap sandbox for Landlock
  (pods that deny user namespaces), and `CODEX_SANDBOX_UNAVAILABLE=true` runs
  Codex with `danger-full-access` on hosts where neither backend works (no
  user namespaces and no `landlock_*` syscalls); the pod boundary and runtime
  command guards remain in force.
- `opencode`: command-backed OpenCode CLI adapter. It writes an OpenCode config
  through `OPENCODE_CONFIG_CONTENT`, runs `opencode run --format json`, consumes
  JSON streaming events, records `opencode_*` artifacts, emits assistant/tool
  live events, and returns the final assistant text. When a workspace lease is
  present, OpenCode runs with `workspace_lease.root_path` as the working
  directory. Delivery actions such as pushing branches or opening pull requests
  remain host/workspace responsibilities.

Native SDK Eino runs currently support Anthropic Claude and OpenAI-compatible
Responses models via Eino:

- `ANTHROPIC_API_KEY` enables the model path.
- `ANTHROPIC_BASE_URL` optionally overrides the Anthropic API base URL.
- `OPENAI_API_KEY` enables `provider: "openai"` agents through the OpenAI
  Responses API.
- `OPENAI_BASE_URL` optionally overrides the OpenAI Responses base URL, which
  defaults to `https://api.openai.com/v1`.
- `OPENROUTER_API_KEY` enables `provider: "openrouter"` or
  `provider: "openrouter_responses"` agents through the Responses-compatible
  path.
- `OPENROUTER_BASE_URL` defaults to `https://openrouter.ai/api/v1`.
- `AGENT_RUNTIME_NATIVE_PROVIDER` defaults to `anthropic`.
- `AGENT_RUNTIME_NATIVE_MODEL` defaults by provider: `claude-opus-4-8` for
  Anthropic, `gpt-5.6-terra` for OpenAI, and `openai/gpt-5.6-terra` for
  OpenRouter.
- Codex and OpenCode resolve omitted models through those same provider
  defaults. Codex defaults its provider to OpenAI; OpenCode and native SDK
  execution default their provider to Anthropic.
- `OPENCODE_PATH` selects the OpenCode CLI binary and defaults to `opencode`.
- `AGENT_RUNTIME_OPENCODE_ROOT` selects the per-run isolated OpenCode home root
  and defaults under the system temp directory.
- OpenCode reuses `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, `OPENAI_API_KEY`,
  `OPENAI_BASE_URL`, `OPENROUTER_API_KEY`, and `OPENROUTER_BASE_URL` according
  to the agent provider.

The native execution loop is a host-neutral Eino tool loop:
it builds a system/user prompt from the agent, run, and target context; exposes
allowed `tools.Registry` definitions to the model; executes tool-call rounds up
to `MaxToolSteps`; runs non-mutating tool calls in parallel; emits assistant and
tool-call live events; appends durable `ToolCall` audit records; and aggregates
token usage, tool summaries, and provider continuation metadata into
`Result.OutputSummary`. For safety, OpenAI
Responses `previous_response_id` replay is not enabled by default because full
transcript replay avoids duplicate provider-owned function-call item IDs on
resumed/tool-followup turns.

For native in-process tools, `Definition.Mutating` is enforced before handler
execution. When the agent approval mode requires approval, the adapter creates a
pending `approval_request` interaction, records the attempted durable `ToolCall`
with `approval_required=true`, returns a tool result explaining the pause, and
stops the round with `WaitForApproval`.

Approval modes are additive and backward compatible: `never` starts immediately
and executes mutating tools without approval; `mutating_tools` starts immediately
but gates each mutating tool; `always` preserves the legacy initial run gate and
also gates mutating tools.

Native SDK also owns generic runtime tool contracts so hosts do not have to
re-register them in every tool pack. When present in `allowed_tools`, the model
sees `update_plan`, `request_user_input`, `request_approval`, and
`request_review_checkpoint` definitions. `update_plan` is non-pausing and writes
a `run_plan` inline artifact plus a `plan_updated` runtime event. Interaction
calls validate the v1 payloads, persist pending `AgentRunInteraction` rows,
write `human_input_request` or `human_approval_request` inline artifacts when an
artifact writer is available, append the normal durable `ToolCall` audit record,
and stop the current tool round with `AwaitingInput` or `WaitForApproval`. Legacy
`request_human_input` and `request_human_approval` names are canonicalized to
the newer tool names, and legacy human-input question payloads remain accepted.
Paused native runs persist `native_messages` in `OutputSummary`; on resume, the
native adapter replays that transcript and appends a user-side resume message
containing the human intent, freeform content, structured response payload, and
external actor ID when provided. Resume requests may include `interaction_id`
to resolve a specific pending interaction and a stable `resume_id` to make host
retries idempotent. `Idempotency-Key` and
`X-Agent-Runtime-Interaction-ID` headers are accepted as aliases. Older hosts
may omit both fields; the engine then resolves the latest pending interaction
and generates a resume correlation ID before restarting lightweight or durable
execution. Temporal workflows retain consumed resume IDs so a duplicate signal
cannot advance a later paused turn.

Native model executions also persist normalized transcript rows. Each
execution writes an `assistant_turn` `AgentRunMessage` with normalized
`content_blocks` and `tool_invocations`; if the final assistant round hands off
to a tool such as `request_user_input` or `request_approval`, trailing
`tool_result` messages are persisted separately. Runtime adapters that persist
their own rich messages set `Result.MessagesPersisted` so the generic engine
does not append a duplicate plain assistant message.

Tool outputs remain fully persisted in messages/tool-call audit records, but
the model replay view is compacted before it reaches Eino. Large file/search/
command outputs are reduced to head/tail content with an omission marker, empty
tool outputs become explicit placeholders, bounded compaction exemptions are
honored, and orphaned tool-result messages are dropped during replay so
providers do not reject invalid tool transcripts.

## Skills

Package: `internal/skills`

`Agent.Skills` is active runtime input, not inert metadata. Before executing a
run, the engine merges the agent's skill refs with refs on its persisted
run-scoped MCP attachments, then resolves them through a `skills.Registry`,
canonicalizes the refs back onto the agent, validates runtime/tool
requirements, compiles model instructions, and passes the resolved skill
definitions and aggregate policy through `runtime.ExecutionContext`.

Skill refs can point at built-in packages by `key`, or workspace skills by
`skill_id`/`version_key` through the host-provided workspace lookup. For key-only
refs, built-ins win before active workspace skills. Duplicate resolved skills
are rejected so policy and instructions are deterministic.

The loader supports a package shape based on `SKILL.md` with YAML frontmatter
plus optional `agents/openai.yaml`. Definitions carry instructions,
human-facing interface metadata, required tools, supported runtimes, and policy.
Policy aggregation merges interaction contracts, collects required completion
interaction kinds, and treats `allow_implicit_invocation: false` as the
conservative winner.

Runtime adapters receive skill state in `ExecutionContext`:

- `SkillRefs`: canonical refs suitable for persistence.
- `SkillDefinitions`: resolved package/workspace definitions.
- `SkillInstructions`: concatenated model instructions.
- `SkillPolicy`: merged runtime policy and interaction contracts.

Native SDK appends `SkillInstructions` to the system prompt. Codex and OpenCode
policy helpers are available for runtime-bridge input contracts and fenced
review checkpoint labels.

Hosts may opt into split skill delivery without changing the public agent
schema by setting `runtime_skill_role` in a skill ref's existing `config`
object. `instruction` refs remain policy-active but are assumed to have already
been compiled into the host-provided system prompt. `available` refs are staged
for on-demand access through `list_available_skills`,
`search_available_skills`, and `read_skill`. If no ref carries this marker, the
legacy behavior above is unchanged: every resolved skill is compiled and
staged for the runtime adapter.

Hosts that store behavior as a complete version-owned prompt do not need to
send instruction refs at all. They may place the independently versioned
interaction policy in `execution_config.runtime_policy`; Agent Runtime merges
that policy with any legacy skill-derived policy. This is additive—agents
without `runtime_policy` keep their existing behavior.

Agent Runtime embeds default system skill packages under
`internal/skills/system` and loads them through `skills.NewDefaultRegistry()`.
The `cmd/agent-runtime` API server and `cmd/agent-runtime-worker` Temporal
worker both install that default registry at startup, so built-in skill refs
such as `approval_protocol`, `code_builder`, `review_agent`, and
`dependency_auditor` resolve without host-specific registration.

The embedded catalog also carries preset bundles for `epic_planner`,
`task_planner`, `documentation_agent`, `code_builder`, and `review_agent`.
Native SDK execution applies phase-aware activation before tool
validation when `preset_key`/`preset` and `planning_stage`/`execution_stage`
are present in agent execution config, run metadata, or run trigger data. This
keeps inactive phase modules out of the prompt and prevents their required tools
from blocking the current phase.

App config can register a host-backed workspace skill lookup:

```json
{
  "apps": [{
    "app_id": "host_app",
    "skill_provider": {
      "transport": "http",
      "base_url": "https://host.internal/agent-runtime/skills",
      "package_base_url": "https://host.internal/agent-runtime/skill-packages",
      "token": "service-token"
    }
  }]
}
```

Host adapter tools are scoped by `app_id`. Runtime-owned tools remain global,
while command and MCP aliases registered by one app are not visible to another
app and cannot overwrite another app's handler. `GET /capabilities?app_id=...`
returns the effective tool catalog for that app.

The runtime calls:

- `POST {base_url}/by-id`
- `POST {base_url}/active-by-key`

Request body:

```json
{
  "app_id": "host_app",
  "agent_id": "agent_123",
  "run_id": "run_123",
  "target": {"type": "task", "id": "task_123"},
  "trigger": {},
  "metadata": {"workspace_id": "workspace_123"},
  "skill_id": "skill_123",
  "key": "workspace_skill"
}
```

`/by-id` requires `skill_id`; `/active-by-key` requires `key`. Hosts may return
`404`/`204` for no match, a direct `WorkspaceSkill` JSON object, or
`{"skill": {...}}`. The skill object mirrors workspace skill fields:
`id`, `key`, `version_key`, `title`, `description`, `source_kind`,
`instructions`, `required_tools`, `supported_runtimes`, `interface`, `policy`,
`package_object_key`, `package_file_name`, `package_checksum`, `package_size`,
and `is_archived`.

If `package_base_url` is configured, workspace/imported skill archives are
loaded with:

- `GET {package_base_url}/objects/{url_path_escaped_package_object_key}`

When a run has a workspace lease, Agent Runtime stages resolved skills under
`{workspace_lease.root_path}/.agent-runtime/skills`. Built-ins are copied from
the embedded package tree, workspace/imported skills are extracted from their
stored zip archive, Markdown tool aliases are rewritten to runtime MCP tool
names, and a `runtime_skill_manifest` artifact records the staged root and
canonical skill refs. Runtime adapters receive the path as
`ExecutionContext.StagedSkillRoot`. Codex command and app-server executions also
sync that tree into the configured Codex skill namespace before the Codex
process starts, matching the configured Codex runtime skill discovery path.
Codex developer instructions advertise the absolute run-scoped
`{CODEX_HOME}/skills/agent-runtime` path and never the repository staging
path, so Codex reads the installed packages through its native skill loader
without constructing invalid `.agent-runtime/skills/agent-runtime/...` paths.
For hosts using split skill delivery, only `available` packages are staged and
Codex accesses them through the canonical runtime tools; they are not copied
into Codex's native discovery directory. This keeps behavior instructions in
the system prompt and prevents optional skills from being loaded eagerly.
OpenCode executions pass the staged root through the generated OpenCode config
`skills.paths`. During
Codex and OpenCode execution, repository-provided `.agents/skills` and
`.codex/skills` directories are temporarily moved aside and restored after the
runtime process exits, preventing checked-out repos from shadowing the
runtime-selected skill package set.

Codex app-server runs map live notifications into host-neutral runtime records:

- `thread/started`, `turn/started`, `item/commandExecution/outputDelta`,
  `error`, and `turn/completed` write `codex_stdout_chunk`,
  `codex_stderr_chunk`, final `codex_stdout`, and final `codex_stderr`
  artifacts.
- `turn/diff/updated` and completed `fileChange` items write normalized
  `codex_diff` artifacts with workspace-local paths.
- `turn/plan/updated` writes a `run_plan` artifact and emits a `plan_updated`
  live event.
- `item/agentMessage/delta` emits assistant-message live events.
- `item/started` and `item/completed` emit tool-call live events and append
  durable `ToolCall` audit records. The persisted input/output JSON includes
  `runtime_kind`, `codex_item_id`, `codex_type`, normalized inputs, summaries,
  status, errors, durations, and command/file/MCP/dynamic results where present.
- `turn/completed` with an error fails the run; `status: interrupted` is treated
  as an interrupted turn.

Codex app-server runs also support `CodexConfig.OpenAIAuthMode=
chatgpt_device_code` with a `CodexAuthStore`. The file-backed implementation
restores/promotes `.codex/auth.json` by `{app_id, tenant_id, provider,
auth_mode}` scope and stores promoted auth encrypted at rest with an
AES-256-GCM/base64 envelope. `DefaultCodexConfigFromEnv`
only enables the file-backed auth store when `AGENT_RUNTIME_CODEX_AUTH_DIR` is
set and either `AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY` or
`CODEX_AUTH_ENCRYPTION_KEY` contains a 32-byte hex-encoded AES key. Codex auth
runs emit `codex_auth_state` artifacts plus an authentication interaction when
Codex reports that ChatGPT sign-in is required.

The HTTP API exposes active ChatGPT device-code auth controls for Codex runs:

- `POST /v1/runs/{run_id}/codex-auth/device-code/start?app_id=...`
- `POST /v1/runs/{run_id}/codex-auth/device-code/cancel?app_id=...`

Durable tool-call history is available at:

- `GET /v1/runs/{run_id}/tool-calls?app_id=...`

## Live Event Streaming

Runtime adapters emit host-neutral live events through `engine.EventSink`.
Without additional configuration the runtime uses `SlogEventSink`, so live
events are logged and durable state remains available through the HTTP list
endpoints for messages, artifacts, interactions, and tool calls.

For backend-facing streaming, enable the NATS JetStream sink:

```bash
AGENT_RUNTIME_EVENT_SINK=nats
AGENT_RUNTIME_NATS_URL=nats://localhost:4222
```

The NATS sink publishes a generic runtime event envelope:

```json
{
  "event_id": "uuid",
  "sent_at": "2026-06-23T00:00:00Z",
  "sequence_no": 1,
  "app_id": "host_app",
  "run_id": "run_123",
  "host_run_id": "helpin_run_123",
  "type": "assistant_message_delta",
  "data": {"text": "hello"}
}
```

API and Temporal worker processes also persist non-delta event envelopes in
`agent_run_events`. `GET /runs/{run_id}/events/history` returns that ordered
timeline. The SSE endpoint replays persisted history, polls the shared store for
worker events, and uses a live NATS bridge for token/reasoning deltas when NATS
is configured. High-volume `*_delta` events are not stored permanently.

Hosts can pass `host_run_id` in `StartRunRequest` when they need to preserve an
application-owned run identifier. The runtime keeps its own `run_id` as the
primary identifier and echoes `host_run_id` on stored runs, NATS events, SSE
events, and event `data` payloads. `POST /v1/runs/{run_id}/cancel` accepts
either identifier so a host can still cancel a run if it did not persist the
runtime-owned ID after a successful start.

Runs emit `usage.checkpoint` when token usage is available before terminal
state. Usage payloads are cumulative per-run gauges, not deltas; consumers
should compare the latest value against the last observed value if they need
incremental metering. Checkpoint and terminal payloads include
`usage_semantic: "cumulative"` when usage is present. Terminal `run.completed`,
`run.failed`, and `run.cancelled` events also include a `usage` object when
`OutputSummary` contains token usage. Hosts that enforce budgets should
subscribe to checkpoints and call cancel when the cumulative total crosses the
host credit line.

Default stream and subject configuration:

- stream: `AGENT_RUNTIME_EVENTS`
- stream subjects: `agent-runtime.events.>`
- publish subject:
  `agent-runtime.events.{app_id}.{run_id}.{event_type}`

Override with:

```bash
AGENT_RUNTIME_NATS_STREAM=AGENT_RUNTIME_EVENTS
AGENT_RUNTIME_NATS_STREAM_SUBJECTS=agent-runtime.events.>
AGENT_RUNTIME_NATS_SUBJECT_TEMPLATE=agent-runtime.events.{app_id}.{run_id}.{event_type}
AGENT_RUNTIME_NATS_CLIENT_NAME=agent-runtime
AGENT_RUNTIME_NATS_ENSURE_STREAM=true
```

`AGENT_RUNTIME_EVENT_SINK=log,nats` emits both logs and NATS events.
`AGENT_RUNTIME_EVENT_SINK=none` disables the global event sinks; explicitly
configured per-app callbacks remain active. NATS is intended for
agent-runtime-to-app-backend streaming; app backends should enforce user and
workspace authorization before forwarding events to browsers over their own
WebSocket/SSE/polling layer.

Host applications can also request filtered HTTP delivery in
`AGENT_RUNTIME_APP_CONFIG`:

```yaml
apps:
  - app_id: usermaven
    event_callbacks:
      - url: http://usermaven-server-svc.default.svc.cluster.local/agent-runtime/events
        token_env: USERMAVEN_INTERNAL_API_SECRET
        event_types: [run.completed, run.failed, run.cancelled, run.paused]
  - app_id: helpin
```

The runtime selects callbacks by the event envelope's `app_id`; the `helpin`
entry above receives no HTTP callbacks. An empty or omitted `event_types` list
matches all event types for that app. App callbacks operate alongside global
log/NATS sinks and use separate credentials per destination. Delivery is at
least once, so receivers must be idempotent.

The older `AGENT_RUNTIME_EVENT_SINK=...,callback` configuration remains a
single global destination and receives events from every app. It is retained
for single-app deployment compatibility; shared deployments should use the
per-app configuration instead.

OpenCode CLI runs map JSON stream output into the same host-neutral runtime
records where possible:

- Raw stdout/stderr chunks write `opencode_stdout_chunk` and
  `opencode_stderr_chunk`; final streams write `opencode_stdout` and
  `opencode_stderr`.
- The generated OpenCode config and prompt are saved as `opencode_config` and
  `opencode_prompt`.
- `text` and text-delta events emit assistant-message live events and become
  the returned assistant text.
- Reasoning, step, and tool events emit `reasoning_message_*`,
  `activity_*`, and `tool_call_*` live events. Tool input snapshots/deltas,
  outputs, errors, and OpenCode duration timestamps are preserved where the CLI
  provides them.
- Tool events append durable `ToolCall` audit records with normalized
  `arguments` when OpenCode provides JSON-shaped tool input.
- Fenced JSON handoffs with `intent: "request_user_input"`,
  `intent: "review_checkpoint"`, or `intent: "approval_request"` create durable
  interactions and pause the run through the normal result flags.
- Completed repository runs with a workspace lease capture `diff`,
  `file_bundle`, and `git_persistence_result` artifacts. Host-prepared
  workspaces get a local commit from the OpenCode adapter; repository-provider
  leases with `finalize_policy: "local_commit"` leave the commit to the
  workspace finalizer to avoid duplicate commits.
- Token usage is summarized in `Result.OutputSummary`.

### Codex App-Server Protocol

The Codex app-server adapter talks to a long-running child process over newline
delimited JSON-RPC on stdin/stdout. Agent Runtime sends:

- `initialize`
- `thread/start` or `thread/resume`
- `turn/start`
- `account/read`, `account/login/start`, and `account/login/cancel` when
  ChatGPT device-code auth is enabled
- run-scoped allowed app tools and runtime interaction tools as
  `dynamicTools` on `thread/start` and `thread/resume`
- `config` with `features.default_mode_request_user_input: true`, so Codex
  exposes its built-in `request_user_input` tool in default mode
- JSON-RPC responses for `item/tool/call` dynamic-tool requests
- JSON-RPC responses for pending approval/input requests

Agent Runtime consumes these notifications:

- `thread/started`
- `turn/started`
- `thread/tokenUsage/updated`
- `turn/diff/updated`
- `turn/plan/updated`
- `item/agentMessage/delta`
- `item/commandExecution/outputDelta`
- `item/started`
- `item/completed`
- `error`
- `turn/completed`
- `account/login/completed`
- `account/updated`

Agent Runtime handles these app-server requests as pause points:

- `item/tool/requestUserInput`
- `item/commandExecution/requestApproval`
- `item/fileChange/requestApproval`
- `item/permissions/requestApproval`

`item/tool/call` requests execute through the same run-scoped gateway used by
the MCP bridge and are answered inline, with two exceptions that also pause
the run: runtime-owned interaction tools (`request_user_input`,
`request_approval`, `request_review_checkpoint`) persist their interaction and
leave the tool call unanswered, and gateway tools that require approval leave
the call unanswered until the human decides. On resume the replayed tool call
receives the human's reply, the approval decision, or the executed tool's real
output; if Codex does not replay it, a fallback turn carries the response
instead.

Paused runs persist the pending JSON-RPC request inside `codex_session_state`.
On resume, Agent Runtime replays the Codex thread, waits for that request, sends
the approval/input response, and continues the turn.

Codex approval policy defaults follow the saved agent's `approval_mode`: agents
with `approval_mode: "never"` start Codex with `approvalPolicy: "never"`, while
other modes default to `on-request`. An explicit runtime approval-policy setting
still overrides that default. Built-in command, file-change, and permissions
requests persist as `command_execution_approval`, `file_change_approval`, and
`permissions_approval`; canonical host decisions such as `approve` are
normalized to Codex-native responses such as `accept` before replay.

When selected `request_review_checkpoint` findings are approved, the session
records the repository state at the checkpoint. A completed turn must change
the repository relative to that state. Runtime issues one corrective turn if
the model only acknowledges the approval, then fails instead of reporting a
false successful completion if the repository is still unchanged.

## Repository Workspaces

Package: `internal/workspace`

Writable repository execution is opt-in. Ordinary targets continue to use target
context and MCP tools only.

Agents opt in with execution config:

```json
{
  "workspace": {
    "mode": "host_prepared"
  }
}
```

Use `mode: "host_prepared"` only when the prepared directory is readable by the
runtime worker. In Kubernetes deployments without a shared volume, use
`mode: "repository"` and let the runtime clone locally from a host-supplied
repository spec.

App config registers a workspace provider. `transport: "http"` keeps workspace
preparation in the host:

```json
{
  "apps": [{
    "app_id": "host_app",
    "context_endpoint": "https://host.internal/agent-runtime/target-context",
    "context_token": "service-token",
    "workspace_provider": {
      "transport": "http",
      "base_url": "https://host.internal/agent-runtime/workspaces",
      "token": "service-token"
    }
  }]
}
```

The runtime calls:

- `POST {base_url}/prepare`
- `POST {base_url}/finalize`
- `POST {base_url}/cleanup`

Prepare returns:

```json
{
  "id": "lease_123",
  "provider": "host",
  "root_path": "/var/lib/agent-runtime-workspaces/run_123/repo",
  "cleanup_policy": "on_terminal",
  "metadata": {}
}
```

Cleanup policies are `always`, `on_terminal`, and `manual`. Leases must not
contain secrets.

Use `transport: "repository"` when the host should only return a repository
spec and Agent Runtime should own clone/checkout/Git identity/finalize:

```json
{
  "apps": [{
    "app_id": "host_app",
    "workspace_provider": {
      "transport": "repository",
      "base_url": "https://host.internal/agent-runtime/workspaces",
      "token": "service-token",
      "root_dir": "/var/lib/agent-runtime-workspaces"
    }
  }]
}
```

In that mode the host implements `POST {base_url}/repository-spec` and returns
`clone_url`, optional auth, `base_branch`, `work_branch`, commit identity,
finalize policy, and metadata. Agent Runtime stores only a redacted copy of the
spec on the lease. Supported repository finalization policies are `none`,
`local_commit`, and `push_branch`. Pull request creation remains host-owned:
use `push_branch`, then create or reconcile the PR from the host terminal-event
finalizer.

## Durable Execution

Package: `internal/engine`

```go
type DurableExecutor interface {
  StartRun(ctx context.Context, run *agentcore.AgentRun) error
  CancelRun(ctx context.Context, run *agentcore.AgentRun) error
  ResumeRun(ctx context.Context, run *agentcore.AgentRun, payload ResumePayload) error
}
```

Package `internal/durable` implements this with Temporal:

- `AgentRunWorkflow`
- `AgentRunActivities`
- `RegisterAgentRunWorker`
- `RunEngine`

The API process reconciles durable runs every 30 seconds. Queued rows older
than 30 seconds are idempotently started by workflow ID, repairing the window
where the database commit succeeded but `ExecuteWorkflow` failed. Stale
`running` or `paused` rows are compared with Temporal; if the workflow is
missing or already closed, the database run is failed with an explicit
consistency error. Failure-state persistence is itself retried by Temporal.

Use `execution_mode=lightweight` for in-process execution and
`execution_mode=durable` for Temporal-backed runs.

`GET /runs/{run_id}/execution` returns sanitized Temporal workflow ID, run ID,
task queue, state, timestamps, history length/size, and transition count for
operator drill-down. `GET /runs/search` provides app-scoped status/search
filtering with bounded limit/offset pagination.

## Tools And MCP

Package: `internal/tools`

```go
type Handler func(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error)
```

Tools are registered with `tools.Registry`. Agents whitelist tools through
`Agent.AllowedTools`; individual runs can further narrow that set with
`StartRunRequest.AllowedTools`.

`tools.NewRegistry()` includes host-neutral workspace tools backed by the run's
`WorkspaceLease.RootPath`: `read_file`, `read_files`, `read_file_range`,
`list_directory`, `search_files`, `ripgrep`, `grep`, `list_symbols`,
`write_file`, `edit_file`, `apply_patch`, `run_command`, `list_commits`,
`create_branch`, and `commit_and_push`. Mutating file tools preserve
read-before-write and stale-file checks; `apply_patch` uses a structured
`*** Begin Patch` grammar and unique-context matching. `run_command`
rejects shell operators, runs only allowlisted programs in the workspace root,
caps output, and uses a bounded timeout. Local git tools preserve
bounded `git log` output and stale `index.lock` recovery; provider-specific PR
creation remains a host integration. `write_file`, `edit_file`, `apply_patch`,
`run_command`, `create_branch`, and `commit_and_push` are marked
`Definition.Mutating` so native and MCP paths route through the same approval
gate when the agent approval mode requires it.

When `execution_config.workspace.access` is `read_only`, `run_command` is
further restricted to inspection-only programs and Git subcommands; arbitrary
interpreters, file operations, mutating Git commands, and diff output files are
rejected.

The default registry also includes host-neutral web tools. `fetch_url` and
`crawl_url` are registered by default with public HTTP(S) host validation and
private/local IP rejection. `web_search_exa` is registered when `EXA_API_KEY` is
configured, and `web_search_brave` is registered when `BRAVE_SEARCH_API_KEY` or
`BRAVE_API_KEY` is configured. Agents still must include these names in
`AllowedTools`, and each run can further narrow exposure with run-level
`allowed_tools`. The allowlist is permission policy, not credential storage: a
durable native-SDK run needs the selected provider key in the worker process.
For Codex runs, either allowed search name enables Codex's built-in live web
search; the external Exa/Brave dynamic tool is additionally exposed when its
runtime credential is configured.

Host/internal command-backed tools use the same registry but delegate execution
to the host:

```go
type CommandToolExecutor interface {
  ExecuteCommand(ctx context.Context, meta CommandExecutionContext, commandName string, input json.RawMessage) (json.RawMessage, error)
}
```

`tools.RegisterCommandTools` registers the shared command metadata
(`create_task`, `update_task_state`, `write_document_content`,
`list_repositories`, CRM enrichment tools, and related PM/Docs commands) against
that executor. The default metadata preserves schemas, categories, and
mutating flags so approval gating remains consistent. `tools.HTTPCommandExecutor`
posts to `POST {base_url}/execute` with:

```json
{
  "meta": {
    "app_id": "host_app",
    "run_id": "run_123",
    "agent_id": "agent_123",
    "workspace_id": "workspace_123",
    "target_type": "task",
    "target_id": "task_123"
  },
  "command_name": "pm.update_task_state",
  "input": {"state_id": "done"}
}
```

The host returns either `{"output": {...}}` or `{"error": "message"}`.

Package: `internal/mcp`

```go
type ToolProvider interface {
  ListTools() ([]Tool, error)
  CallTool(name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error)
}
```

Use `mcp.RegisterProviderTools` to expose external MCP tools inside the runtime.
Use `mcp.Gateway` or `cmd/agent-runtime-mcp-bridge` to expose runtime tools to
MCP clients.

For simple HTTP providers, agent-runtime sends `POST {url}/call` with trusted
run metadata:

```json
{
  "tool_name": "search_articles",
  "input": {"query": "pricing"},
  "meta": {
    "app_id": "host_app",
    "run_id": "run_123",
    "agent_id": "agent_123",
    "external_actor_id": "user_123",
    "workspace_id": "workspace_123",
    "target_type": "workspace",
    "target_id": "workspace_123"
  }
}
```

This simple HTTP provider contract is for trusted backend adapters registered in
agent-runtime app config. It is not the public authorization shape for an
external MCP server. Public HTTP MCP servers should follow the MCP authorization
specification: validate `Authorization: Bearer <access-token>` on every request,
bind tokens to the MCP server resource/audience, use scopes for client
capability, apply application RBAC server-side, and avoid token passthrough.

References:

- <https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization>
- <https://modelcontextprotocol.io/docs/tutorials/security/security_best_practices>

Configured backend MCP providers:

```json
{
  "apps": [{
    "app_id": "host_app",
    "mcp_providers": [{
      "name": "content",
      "transport": "http",
      "url": "https://host.internal/agent-runtime/mcp/content",
      "token": "service-token",
      "tool_prefix": "content",
      "allowed_tools": ["search_articles", "read_article"]
    }]
  }]
}
```

Supported transports are `http` for SDK/FastAPI providers, `streamable_http`
for JSON-RPC MCP servers, and `stdio` for command-backed MCP servers. If
`transport` is omitted, `http` is used.

Configured backend command provider:

```json
{
  "apps": [{
    "app_id": "host_app",
    "command_provider": {
      "transport": "http",
      "base_url": "https://host.internal/agent-runtime/commands",
      "token": "service-token"
    }
  }]
}
```

## HTTP API

Base command: `cmd/agent-runtime`

Primary versioned routes are documented in `docs/openapi.yaml`.

Use `/v1/...` for new clients. `/internal/...` remains as the legacy alias for
the current service and is protected by the same service-token middleware.

`AGENT_RUNTIME_SERVICE_TOKEN` is required for bearer auth on `/v1` routes,
legacy `/internal` aliases, and internal tool gateway endpoints. The API fails
closed at startup when the token is absent unless
`AGENT_RUNTIME_ALLOW_ANONYMOUS=true` is explicitly set for isolated local
development.

## Python SDK

Repository: `github.com/helpin-ai/agent-runtime-python`

```python
from agent_runtime import AgentRuntimeClient

client = AgentRuntimeClient(
    base_url="https://agent-runtime.internal",
    app_id="host_app",
    service_token="service-token",
)

runs = client.list_runs()
```

The Python SDK also exposes `get_agent`, `update_agent`, and `upsert_agent` for
app-owned agent registry bootstrap. Current observability helpers include
`get_capabilities`, `get_app_health`, `search_runs`, `list_run_events`,
`get_run_execution`, and the live `iter_run_events` SSE iterator. Resume
requests accept `resume_id` and `interaction_id` for retry-safe interaction
resolution.

For FastAPI target-context adapters:

```python
from agent_runtime import create_fastapi_target_context_router

app.include_router(create_fastapi_target_context_router(resolve_context, token="service-token"))
```

Python hosts can receive per-app callbacks with
`create_fastapi_event_callback_router`. NATS/JetStream consumers can install
the optional `agent-runtime[nats]` extra and use `NATSConsumer` with the same
stream, subject, acknowledgement, and retry conventions as the Go SDK.

## React Package

Package: `packages/react`

Exports:

- `createAgentRuntimeClient`
- `useAgentRun`
- `AgentRunPanel`
- `AgentRunView`
- status helpers for run display logic

Host apps should proxy agent-runtime requests through their own backend for user
auth, or inject a service token only in trusted internal UI surfaces.
