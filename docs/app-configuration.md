# Host app configuration

`AGENT_RUNTIME_APP_CONFIG` connects each product to its target context, domain
commands, skills, MCP servers, and workspace provider. JSON remains supported;
YAML files are easier to review and can reference token environment variables.

```bash
AGENT_RUNTIME_APP_CONFIG=@/etc/agent-runtime/apps.yaml
HELPIN_INTERNAL_API_SECRET=...
```

```yaml
apps:
  - app_id: helpin
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
```

Only `app_id` is always required. Provider blocks are optional:

| Block | Use it when |
| --- | --- |
| `context_endpoint` | Targets need product-owned context beyond their ID. |
| `command_provider` | Agents call product-domain tools such as task or document commands. |
| `skill_provider` | Skills are stored or versioned by the host product. |
| `workspace_provider` | Runs need host-authorized repository checkout and delivery. |
| `mcp_providers` | The app supplies additional MCP tools. |

Tokens may still be placed directly in JSON for backward compatibility. Prefer
`context_token_env`, `token_env`, and `package_token_env` so configuration can
be reviewed without copying secrets into a nested JSON value.

Every provider block is validated for a supported transport and a usable URL.
MCP HTTP providers also accept `token_env`; stdio providers require `command`
instead of `url`.

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
