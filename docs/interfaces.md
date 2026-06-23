# Agent Runtime Interfaces

This project is an internal, host-neutral agent runtime. Host apps plug in through
small interfaces instead of forking the executor.

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

The SQL store includes Helpin-ported sanitization for PostgreSQL-hostile null
bytes and invalid JSON.

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
    "app_id": "contentpen",
    "context_endpoint": "https://contentpen.internal/agent-runtime/target-context",
    "context_token": "service-token"
  }]
}
```

Set this JSON directly in `AGENT_RUNTIME_APP_CONFIG`, or set
`AGENT_RUNTIME_APP_CONFIG=@/path/to/config.json`.

Target-context request:

```json
{
  "app_id": "contentpen",
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

- `native_sdk`: in-process adapter. Without model configuration it preserves the
  deterministic local fallback. With `ANTHROPIC_API_KEY` or
  `AGENT_RUNTIME_NATIVE_EINO=true`, it uses the Eino-backed native execution
  loop with registered tools.
- `codex`: command-backed adapter when `CODEX_PATH` or explicit config is set;
  deterministic fallback when not configured. If a workspace lease is present,
  command-backed Codex runs with `workspace_lease.root_path` as its working
  directory. `CodexConfig.AppServer=true` enables the Codex app-server stdio
  protocol path for `initialize`, `thread/start`, `turn/start`, streaming
  notifications, and approval/input pauses. App-server runs persist
  `codex_session_state` artifacts so paused approval/input requests can resume
  the same Codex thread.
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
- `AGENT_RUNTIME_NATIVE_MODEL` defaults by provider: `claude-sonnet-4-6` for
  Anthropic, `gpt-4.1` for OpenAI, and `openai/gpt-4.1` for OpenRouter.
- `OPENCODE_PATH` selects the OpenCode CLI binary and defaults to `opencode`.
- `AGENT_RUNTIME_OPENCODE_ROOT` selects the per-run isolated OpenCode home root
  and defaults under the system temp directory.
- OpenCode reuses `ANTHROPIC_API_KEY`, `ANTHROPIC_BASE_URL`, `OPENAI_API_KEY`,
  `OPENAI_BASE_URL`, `OPENROUTER_API_KEY`, and `OPENROUTER_BASE_URL` according
  to the agent provider.

The native execution loop is the host-neutral lift of Helpin's Eino tool loop:
it builds a system/user prompt from the agent, run, and target context; exposes
allowed `tools.Registry` definitions to the model; executes tool-call rounds up
to `MaxToolSteps`; runs non-mutating tool calls in parallel; emits assistant and
tool-call live events; appends durable `ToolCall` audit records; and aggregates
token usage, tool summaries, and provider continuation metadata into
`Result.OutputSummary`. Matching Helpin's current safety behavior, OpenAI
Responses `previous_response_id` replay is not enabled by default because full
transcript replay avoids duplicate provider-owned function-call item IDs on
resumed/tool-followup turns.

For native in-process tools, `Definition.Mutating` is enforced before handler
execution. When the agent approval mode requires approval, the adapter creates a
pending `approval_request` interaction, records the attempted durable `ToolCall`
with `approval_required=true`, returns a tool result explaining the pause, and
stops the round with `WaitForApproval`.

Native SDK also owns the generic Helpin interaction tool contracts so hosts do
not have to re-register them in every tool pack. When present in
`allowed_tools`, the model sees `request_user_input`, `request_approval`, and
`request_review_checkpoint` definitions. Calls validate the Helpin v1 payloads,
persist pending `AgentRunInteraction` rows, write `human_input_request` or
`human_approval_request` inline artifacts when an artifact writer is available,
append the normal durable `ToolCall` audit record, and stop the current tool
round with `AwaitingInput` or `WaitForApproval`. Legacy
`request_human_input` and `request_human_approval` names are canonicalized to
the newer tool names, and legacy human-input question payloads remain accepted.
Paused native runs persist `native_messages` in `OutputSummary`; on resume, the
native adapter replays that transcript and appends a user-side resume message
containing the human intent, freeform content, structured response payload, and
external actor ID when provided. The engine resolves the latest pending
interaction with the resume response payload before restarting lightweight or
durable execution.

Native model executions also persist Helpin-shaped transcript rows. Each
execution writes an `assistant_turn` `AgentRunMessage` with normalized
`content_blocks` and `tool_invocations`; if the final assistant round hands off
to a tool such as `request_user_input` or `request_approval`, trailing
`tool_result` messages are persisted separately. Runtime adapters that persist
their own rich messages set `Result.MessagesPersisted` so the generic engine
does not append a duplicate plain assistant message.

Tool outputs remain fully persisted in messages/tool-call audit records, but
the model replay view is compacted before it reaches Eino. Large file/search/
command outputs are reduced to head/tail content with an omission marker, empty
tool outputs become explicit placeholders, bounded `_helpin_compaction` and
`_agent_runtime_compaction` exemptions are honored, and orphaned tool-result
messages are dropped during replay so providers do not reject invalid tool
transcripts.

## Skills

Package: `internal/skills`

`Agent.Skills` is active runtime input, not inert metadata. Before executing a
run, the engine resolves the agent's skill refs through a `skills.Registry`,
canonicalizes the refs back onto the agent, validates runtime/tool
requirements, compiles model instructions, and passes the resolved skill
definitions and aggregate policy through `runtime.ExecutionContext`.

Skill refs can point at built-in packages by `key`, or workspace skills by
`skill_id`/`version_key` through the host-provided workspace lookup. For key-only
refs, built-ins win before active workspace skills. Duplicate resolved skills
are rejected so policy and instructions are deterministic.

The loader supports the Helpin package shape: `SKILL.md` with YAML frontmatter
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
review checkpoint labels, matching the Helpin migration path.

Agent Runtime embeds the current Helpin system skill packages under
`internal/skills/system` and loads them through `skills.NewDefaultRegistry()`.
The `cmd/agent-runtime` API server and `cmd/agent-runtime-worker` Temporal
worker both install that default registry at startup, so built-in skill refs
such as `approval_protocol`, `code_builder`, `review_agent`, and
`dependency_auditor` resolve without host-specific registration.

The embedded catalog also carries Helpin preset bundles for `epic_planner`,
`task_planner`, `documentation_agent`, `code_builder`, and `review_agent`.
Native SDK execution applies Helpin's phase-aware activation before tool
validation when `preset_key`/`preset` and `planning_stage`/`execution_stage`
are present in agent execution config, run metadata, or run trigger data. This
keeps inactive phase modules out of the prompt and prevents their required tools
from blocking the current phase.

App config can register a host-backed workspace skill lookup:

```json
{
  "apps": [{
    "app_id": "helpin",
    "skill_provider": {
      "transport": "http",
      "base_url": "https://helpin.internal/agent-runtime/skills",
      "package_base_url": "https://helpin.internal/agent-runtime/skill-packages",
      "token": "service-token"
    }
  }]
}
```

The runtime calls:

- `POST {base_url}/by-id`
- `POST {base_url}/active-by-key`

Request body:

```json
{
  "app_id": "helpin",
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
`{"skill": {...}}`. The skill object mirrors Helpin workspace skill fields:
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
stored zip archive, Markdown tool aliases are rewritten to Helpin MCP runtime
tool names, and a `runtime_skill_manifest` artifact records the staged root and
canonical skill refs. Runtime adapters receive the path as
`ExecutionContext.StagedSkillRoot`. Codex command and app-server executions also
sync that tree into `CODEX_HOME/skills/helpin` before the Codex process starts,
matching Helpin's Codex runtime skill discovery path. OpenCode executions pass
the staged root through the generated OpenCode config `skills.paths`. During
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
auth_mode}` scope and stores promoted auth encrypted at rest with the
Helpin-compatible AES-256-GCM/base64 envelope. `DefaultCodexConfigFromEnv`
only enables the file-backed auth store when `AGENT_RUNTIME_CODEX_AUTH_DIR` is
set and either `AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY` or
`CODEX_AUTH_ENCRYPTION_KEY` contains a 32-byte hex-encoded AES key. Codex auth
runs emit `codex_auth_state` artifacts plus an authentication interaction when
Codex reports that ChatGPT sign-in is required.

The HTTP API exposes active ChatGPT device-code auth controls for Codex runs:

- `POST /internal/runs/{run_id}/codex-auth/device-code/start?app_id=...`
- `POST /internal/runs/{run_id}/codex-auth/device-code/cancel?app_id=...`
- The same paths are available under `/v1/runs/...` with service-token auth.

Durable tool-call history is available at:

- `GET /internal/runs/{run_id}/tool-calls?app_id=...`
- `GET /v1/runs/{run_id}/tool-calls?app_id=...`

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

Paused runs persist the pending JSON-RPC request inside `codex_session_state`.
On resume, Agent Runtime replays the Codex thread, waits for that request, sends
the approval/input response, and continues the turn.

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

App config registers a workspace provider. `transport: "http"` keeps workspace
preparation in the host:

```json
{
  "apps": [{
    "app_id": "helpin",
    "context_endpoint": "https://helpin.internal/agent-runtime/target-context",
    "context_token": "service-token",
    "workspace_provider": {
      "transport": "http",
      "base_url": "https://helpin.internal/agent-runtime/workspaces",
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
  "provider": "helpin",
  "root_path": "/var/lib/helpin-agent-workspaces/run_123/repo",
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
    "app_id": "helpin",
    "workspace_provider": {
      "transport": "repository",
      "base_url": "https://helpin.internal/agent-runtime/workspaces",
      "token": "service-token",
      "root_dir": "/var/lib/agent-runtime-workspaces"
    }
  }]
}
```

In that mode the host implements `POST {base_url}/repository-spec` and returns
`clone_url`, optional auth, `base_branch`, `work_branch`, commit identity,
finalize policy, and metadata. Agent Runtime stores only a redacted copy of the
spec on the lease.

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

Use `execution_mode=lightweight` for in-process execution and
`execution_mode=durable` for Temporal-backed runs.

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
`create_branch`, and `commit_and_push`. Mutating file tools preserve Helpin's
read-before-write and stale-file checks; `apply_patch` uses the same structured
`*** Begin Patch` grammar and unique-context matching as Helpin. `run_command`
rejects shell operators, runs only allowlisted programs in the workspace root,
caps output, and uses a bounded timeout. Local git tools preserve Helpin's
bounded `git log` output and stale `index.lock` recovery; provider-specific PR
creation remains a host integration. `write_file`, `edit_file`, `apply_patch`,
`run_command`, `create_branch`, and `commit_and_push` are marked
`Definition.Mutating` so native and MCP paths route through the same approval
gate when the agent approval mode requires it.

Host/internal command-backed tools use the same registry but delegate execution
to the host:

```go
type CommandToolExecutor interface {
  ExecuteCommand(ctx context.Context, meta CommandExecutionContext, commandName string, input json.RawMessage) (json.RawMessage, error)
}
```

`tools.RegisterCommandTools` registers the shared Helpin command metadata
(`create_task`, `update_task_state`, `write_document_content`,
`list_repositories`, CRM enrichment tools, and related PM/Docs commands) against
that executor. The default metadata preserves Helpin schemas, categories, and
mutating flags so approval gating remains consistent. `tools.HTTPCommandExecutor`
posts to `POST {base_url}/execute` with:

```json
{
  "meta": {
    "app_id": "helpin",
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
  CallTool(name string, input json.RawMessage) (*CallResult, error)
}
```

Use `mcp.RegisterProviderTools` to expose external MCP tools inside the runtime.
Use `mcp.Gateway` or `cmd/agent-runtime-mcp-bridge` to expose runtime tools to
MCP clients.

Configured backend MCP providers:

```json
{
  "apps": [{
    "app_id": "contentpen",
    "mcp_providers": [{
      "name": "content",
      "transport": "streamable_http",
      "url": "https://contentpen.internal/mcp",
      "token": "service-token",
      "tool_prefix": "content",
      "allowed_tools": ["search_articles", "read_article"]
    }]
  }]
}
```

Supported transports are `streamable_http` and `stdio`.

Configured backend command provider:

```json
{
  "apps": [{
    "app_id": "helpin",
    "command_provider": {
      "transport": "http",
      "base_url": "https://helpin.internal/agent-runtime/commands",
      "token": "service-token"
    }
  }]
}
```

## HTTP API

Base command: `cmd/agent-runtime`

Primary versioned routes are documented in `docs/openapi.yaml`.

Use `/v1/...` for new clients. `/internal/...` remains as the legacy internal
alias for the current service.

Set `AGENT_RUNTIME_SERVICE_TOKEN` to require bearer auth on `/v1` routes and
internal tool gateway endpoints.

## Python SDK

Package: `packages/python`

```python
from agent_runtime import AgentRuntimeClient

client = AgentRuntimeClient(
    base_url="https://agent-runtime.internal",
    app_id="contentpen",
    service_token="service-token",
)

runs = client.list_runs()
```

For FastAPI target-context adapters:

```python
from agent_runtime import create_fastapi_target_context_router

app.include_router(create_fastapi_target_context_router(resolve_context, token="service-token"))
```

## React Package

Package: `packages/react`

Exports:

- `createAgentRuntimeClient`
- `useAgentRun`
- `AgentRunPanel`
- `AgentRunView`
- status helpers from Helpin’s run display logic

Host apps should proxy agent-runtime requests through their own backend for user
auth, or inject a service token only in trusted internal UI surfaces.
