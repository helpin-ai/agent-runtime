# Agent Runtime

Internal agent execution engine extracted from Helpin.

This repository is intentionally host-neutral. Apps such as Helpin, UserMaven,
and ContentPen plug in by registering app-scoped agents, targets, context
providers, MCP tools, and optional tool packs.

## What is implemented

- App-scoped agent and run models.
- Generic target contract.
- In-memory store for local development and tests.
- GORM SQL store for Postgres/sqlite with Helpin-ported JSON/null-byte
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
[docs/repository-workspaces.md](docs/repository-workspaces.md) for the Helpin
coding-agent port boundary.

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

React package:

```bash
cd packages/react
npm test
```

Python SDK:

```bash
cd packages/python
PYTHONPATH=. python3 -m unittest discover -s tests
```

Key environment variables:

- `AGENT_RUNTIME_STORE_DRIVER`: `memory`, `sqlite`, or `postgres`
- `DATABASE_URL`: Postgres DSN when using Postgres
- `AGENT_RUNTIME_SERVICE_TOKEN`: bearer token for `/v1` service API
- `AGENT_RUNTIME_APP_CONFIG`: JSON app adapter/MCP config, or `@/path/file.json`
- `ANTHROPIC_API_KEY`: enables Eino-backed Anthropic `native_sdk` execution
- `OPENAI_API_KEY`: enables Eino-backed OpenAI Responses `native_sdk` execution
- `OPENROUTER_API_KEY`: enables Eino-backed OpenRouter Responses `native_sdk` execution
- `AGENT_RUNTIME_NATIVE_MODEL`: optional native SDK model override
- `TEMPORAL_ADDRESS`: enables durable Temporal execution
- `TEMPORAL_NAMESPACE`: Temporal namespace, defaults to `default`

## API examples

Create an agent:

```bash
curl -s localhost:8090/internal/agents -d '{
  "app_id": "helpin",
  "name": "Task Agent",
  "runtime_kind": "native_sdk",
  "system_prompt": "Help with the target.",
  "allowed_tools": ["get_context"],
  "allowed_targets": ["task"]
}'
```

Start a run:

```bash
curl -s localhost:8090/internal/runs -d '{
  "app_id": "helpin",
  "agent_id": "agent_id_from_create",
  "target": {"type": "task", "id": "task_123"},
  "instructions": "Summarize this target."
}'
```
