# Repository Workspaces

Repository targets are special because writable coding agents need more than
read-only context. They need a prepared filesystem, credentials, branch policy,
session state, and delivery bookkeeping. Agent Runtime supports both
host-prepared workspaces and generic repository workspaces where the host
returns a repository spec and Agent Runtime owns clone/checkout/finalize.

## Modes

Most apps should expose repository or codebase data as read-only MCP tools. That
is enough for analysis, summaries, release notes, docs updates, and support
context.

Writable coding runs opt in per agent:

```json
{
  "workspace": {
    "mode": "host_prepared"
  }
}
```

When this is set, the runtime asks the configured app workspace provider to
prepare a workspace before invoking the runtime adapter. Without this setting,
no workspace provider is called.

The configured provider decides whether the workspace is host-prepared or
runtime-prepared from a repository spec.

## Provider Configuration

Configure the provider with `AGENT_RUNTIME_APP_CONFIG`:

```json
{
  "apps": [{
    "app_id": "helpin",
    "workspace_provider": {
      "transport": "http",
      "base_url": "https://helpin.internal/agent-runtime/workspaces",
      "token": "service-token"
    }
  }]
}
```

For a host-prepared provider, the runtime calls three endpoints.

`POST /prepare`

```json
{
  "app_id": "helpin",
  "run_id": "run_123",
  "agent_id": "agent_123",
  "runtime_kind": "codex",
  "target": {"type": "task", "id": "task_123"},
  "target_context": {},
  "instructions": "Implement the task",
  "trigger": {},
  "metadata": {},
  "workspace_mode": "host_prepared",
  "execution_config": {}
}
```

Response:

```json
{
  "id": "lease_123",
  "provider": "helpin",
  "root_path": "/var/lib/helpin-agent-workspaces/run_123/repo",
  "cleanup_policy": "on_terminal",
  "metadata": {
    "repository_id": "repo_123",
    "base_branch": "main",
    "working_branch": "task/123-fix-login"
  }
}
```

`POST /finalize`

```json
{
  "app_id": "helpin",
  "run_id": "run_123",
  "agent_id": "agent_123",
  "runtime_kind": "codex",
  "target": {"type": "task", "id": "task_123"},
  "lease": {},
  "outcome": "completed",
  "output_summary": {}
}
```

The provider may return an updated `output_summary` with delivery metadata such
as commit SHA, PR URL, or branch status.

`POST /cleanup`

```json
{
  "app_id": "helpin",
  "run_id": "run_123",
  "agent_id": "agent_123",
  "runtime_kind": "codex",
  "target": {"type": "task", "id": "task_123"},
  "lease": {},
  "reason": "completed"
}
```

Supported cleanup policies:

- `on_terminal`: cleanup after completed, failed, or cancelled runs.
- `always`: cleanup after every adapter execution, including pauses.
- `manual`: runtime never auto-cleans the workspace.

## Repository Spec Provider

Use `transport: "repository"` when Agent Runtime should own local Git
workspace preparation:

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

The runtime calls `POST /repository-spec` with the same prepare request shape.
The host responds with:

```json
{
  "provider": "github",
  "clone_url": "https://github.com/acme/repo.git",
  "auth": {"type": "github", "token": "installation-token"},
  "base_branch": "main",
  "work_branch": "agent/run-123",
  "commit_identity": {
    "name": "Helpin Agent",
    "email": "agent@helpin.ai"
  },
  "finalize_policy": "local_commit",
  "metadata": {
    "commit_message": "Implement task TASK-123"
  }
}
```

Agent Runtime clones the repository, checks out the work branch from the base
branch, configures Git identity, returns a workspace lease, and can finalize a
completed run with a local commit. Push and PR finalization are intentionally
left as explicit follow-up policies.

For OpenCode runs, the runtime adapter captures and stages repository changes
before engine workspace finalization. It writes `diff`, `file_bundle`, and
`git_persistence_result` artifacts. When the lease comes from the generic
repository provider with `finalize_policy: "local_commit"`, OpenCode leaves the
actual commit to the provider finalizer. Host-prepared workspaces receive a
local commit inside the OpenCode adapter, matching Helpin's current local-commit
runtime behavior while keeping push/PR delivery outside the generic runtime.

## Helpin Porting Notes

Helpin should keep product-specific resolution behind provider endpoints instead
of moving Helpin tables into Agent Runtime.

Map the current flow this way:

- Target context endpoint resolves task/epic/repository display and read-only
  context.
- Repository spec resolves task delivery target, repo credentials, base branch,
  working branch, and delivery metadata.
- Agent Runtime owns checkout, branch preparation, Git identity, command guards,
  and generic finalize behavior.
- Codex runs inside the returned `root_path`.
- Helpin still owns task git link updates, PR metadata reconciliation, and
  product notifications.
- Workspace `cleanup` removes or preserves local files based on Helpin’s
  operational policy.

The runtime should not know Helpin workspace IDs, task delivery tables, GitHub
installation rows, team defaults, PR reconciliation rules, or Codex auth storage.

## Non-Helpin Apps

UserMaven, ContentPen, and other apps should not configure a workspace provider
unless they need writable repository execution. They can expose codebase or repo
facts through read-only MCP tools and keep normal target context behavior.
