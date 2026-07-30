# Run-scoped MCP servers

Host applications can attach workspace-selected MCP servers to an individual
agent run. This is intentionally an execution contract, not an MCP installation
or credential-management API.

## Ownership boundary

The host app, such as Helpin, owns:

- adding and removing MCP servers from a workspace;
- the workspace-to-server permission model;
- browser OAuth, callback handling, consent, and account selection;
- encrypted refresh-token or API-key storage and revocation;
- selecting the exact MCP tools available to a run; and
- refreshing or exchanging a credential before calling Agent Runtime.

Agent Runtime owns:

- strict request and URL validation;
- encryption of the run credential before persistence;
- standards-compliant MCP initialization and Streamable HTTP sessions;
- exposing only the tools selected by the app;
- read/write classification, approval routing, and tool-call audit records;
- isolated tool registration for Native, Codex, and OpenCode runs; and
- clearing the encrypted credential when the run becomes terminal.

Agent Runtime does not open a browser, persist workspace MCP installations, or
store a long-lived OAuth refresh token. Do not send a Helpin login token or any
other token issued for a different audience to an MCP server.

## App flow

1. A workspace administrator adds an MCP server in the app.
2. The app completes any browser OAuth flow and stores the resulting long-lived
   credential in its own encrypted credential store.
3. When starting a run, the app authorizes the selected server and tools for
   that workspace and user.
4. The app refreshes or exchanges the credential for an MCP-resource-bound,
   preferably short-lived access token.
5. The app calls `POST /v1/runs` through an Agent Runtime SDK and includes
   `mcp_servers`.
6. Agent Runtime validates and encrypts the credential, then connects from the
   process that executes the run.

Apps can inspect `GET /v1/capabilities`. `run_mcp.supported` advertises the
feature and `run_mcp.credential_encryption_configured` confirms whether the
deployment can accept credential-bearing attachments without exposing the key.

The app should choose an access-token lifetime that covers the expected run and
any planned pause/resume window. This version has no credential-refresh callback
during a run. An expired credential fails execution explicitly; the app must
start a new run with a fresh credential. This avoids sending refresh tokens to
the execution service.

## Request contract

```json
{
  "app_id": "helpin",
  "agent_id": "agent_123",
  "target": {"type": "workspace", "id": "workspace_123"},
  "instructions": "Summarize the linked GitHub issue.",
  "mcp_servers": [
    {
      "server_id": "workspace_mcp_456",
      "server_name": "github",
      "transport": "streamable_http",
      "url": "https://mcp.example.com/mcp",
      "tools": [
        {"name": "get_issue", "access": "read"},
        {"name": "create_issue", "access": "write"}
      ],
      "credential": {
        "type": "bearer_token",
        "access_token": "short-lived-secret",
        "expires_at": "2026-07-30T18:00:00Z"
      }
    }
  ]
}
```

Every field in `mcp_servers` is validated strictly; unknown fields are rejected.
One request may attach at most 16 servers and 128 tools per server. Start-run
bodies are capped at 1 MiB, individual credentials at 64 KiB, and MCP responses
at 16 MiB.

| Field | Contract |
| --- | --- |
| `server_id` | Stable app-owned identity for the workspace MCP installation; unique within the run. |
| `server_name` | Human-readable, stable name used to build runtime tool aliases. |
| `transport` | Must be `streamable_http`. Request-supplied commands and `stdio` are not accepted. |
| `url` | Absolute HTTPS URL. HTTP is disabled by default. Userinfo, query parameters, and fragments are rejected so credentials cannot enter the persisted URL. |
| `tools` | Non-empty, exact allowlist. Agent Runtime verifies each name is advertised by the server. |
| `tools[].access` | `read` or `write`; `write` tools use the runtime's normal mutating-tool approval path. |
| `credential` | Optional for public MCP servers. Supports `bearer_token` or `headers`. |

The runtime alias for a tool is
`mcp__{sanitized_server_name}__{sanitized_tool_name}`. Run MCP aliases are
automatically added to that run's effective tool set; they do not need to be in
the saved agent's `allowed_tools` and cannot leak into another run.

For API-key MCPs, use the header credential shape:

```json
{
  "type": "headers",
  "headers": {"X-API-Key": "secret"},
  "expires_at": "2026-07-30T18:00:00Z"
}
```

Connection and MCP protocol headers such as `Host`, `Content-Length`,
`Mcp-Session-Id`, `Mcp-Protocol-Version`, and `Origin` cannot be supplied as
credential headers.

## Go SDK

```go
run, err := client.StartRun(ctx, sdk.StartRunRequest{
    AgentID: "agent_123",
    Target: sdk.TargetRef{Type: "workspace", ID: "workspace_123"},
    MCPServers: []sdk.RunMCPServer{{
        ServerID:   "workspace_mcp_456",
        ServerName: "github",
        Transport:  sdk.MCPTransportStreamableHTTP,
        URL:        "https://mcp.example.com/mcp",
        Tools: []sdk.RunMCPTool{
            {Name: "get_issue", Access: sdk.MCPToolAccessRead},
            {Name: "create_issue", Access: sdk.MCPToolAccessWrite},
        },
        Credential: &sdk.RunMCPCredential{
            Type:        sdk.MCPCredentialBearerToken,
            AccessToken: accessToken,
            ExpiresAt:   &expiresAt,
        },
    }},
})
```

## Python SDK

```python
from agent_runtime import (
    AgentRuntimeClient,
    RunMCPCredential,
    RunMCPServer,
    RunMCPTool,
    StartRunRequest,
)

run = client.start_run(StartRunRequest(
    agent_id="agent_123",
    target={"type": "workspace", "id": "workspace_123"},
    mcp_servers=[RunMCPServer(
        server_id="workspace_mcp_456",
        server_name="github",
        url="https://mcp.example.com/mcp",
        tools=[
            RunMCPTool(name="get_issue", access="read"),
            RunMCPTool(name="create_issue", access="write"),
        ],
        credential=RunMCPCredential(
            type="bearer_token",
            access_token=access_token,
            expires_at=expires_at,
        ),
    )],
))
```

The SDK sends credentials only in the start request. `AgentRun`, list/search
responses, events, messages, artifacts, and tool-call records do not contain
the credential.

## Runtime deployment

The API and every Temporal worker that can execute a run must use the same
configuration:

| Environment variable | Purpose |
| --- | --- |
| `AGENT_RUNTIME_MCP_CREDENTIAL_ENCRYPTION_KEY` | Required when a run supplies a credential. Exactly 32 raw bytes or base64-encoded 32 bytes. |
| `AGENT_RUNTIME_MCP_ALLOWED_HOSTS` | Optional comma-separated exact hosts or `*.example.com` suffix patterns. When set, every run MCP URL must match. |
| `AGENT_RUNTIME_MCP_ALLOW_PRIVATE_NETWORKS` | Default `false`. Set only when trusted MCP servers intentionally resolve to private/loopback/link-local ranges. |
| `AGENT_RUNTIME_MCP_ALLOW_HTTP` | Default `false`. Local-development escape hatch; production MCP URLs should use HTTPS. |

Private/local network destinations are denied during DNS resolution by default,
redirects cannot change hosts, and credentials are attached only by the
runtime's HTTP transport. Prefer an explicit host allowlist in shared
deployments. If private cluster MCP services are required, combine
`AGENT_RUNTIME_MCP_ALLOW_PRIVATE_NETWORKS=true` with a narrow allowed-host list
and network policy.

Rotate the encryption key only after active and paused runs using the old key
are terminal, or deploy a coordinated migration. The ciphertext is bound to
`app_id`, `run_id`, and `server_id`, so moving it between records cannot produce
a valid credential.

## Runtime behavior

- Native and Codex use an isolated in-process registry created for the run.
- OpenCode connects to a token-protected loopback MCP broker. Its saved config
  contains an environment-variable reference, never the broker token or remote
  MCP credential.
- Calls pass through the same gateway as built-in tools, producing normal
  approval interactions and `agent_run_tool_calls` audit records.
- Paused runs retain the encrypted credential so they can resume. Completed,
  failed, and cancelled runs keep only the non-secret server/tool summary; the
  encrypted credential column is cleared.

Static `mcp_providers` in `AGENT_RUNTIME_APP_CONFIG` remain useful for
deployment-owned backend integrations shared by many runs. Use `mcp_servers` on
`StartRunRequest` for user/workspace-selected integrations and credentials.

## Failure cases

Run creation returns `400` for malformed URLs, unsupported transports,
duplicate IDs or aliases, empty tool lists, invalid access classifications,
expired credentials, forbidden headers, or missing encryption configuration.
Execution fails with a server-specific error when the URL cannot be reached,
the MCP handshake fails, the credential has expired, or an authorized tool is
not advertised by the server.

Relevant MCP references:

- [MCP authorization](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)
- [MCP security best practices](https://modelcontextprotocol.io/docs/tutorials/security/security_best_practices)
