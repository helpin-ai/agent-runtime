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

The app should choose an access-token lifetime that covers the expected active
turn. Before resuming a paused run, the app can replace an expired access token
through the credential-rotation endpoint below. Agent Runtime never receives a
refresh token.

## What the SDK, application, and Runtime each provide

| Layer | Provides | Deliberately does not provide |
| --- | --- | --- |
| Go/Python SDK OAuth helper | Protected-resource and authorization-server discovery, PKCE S256 state/verifier generation, Dynamic Client Registration, authorization URL construction, code exchange, refresh, resource audience, supported client authentication methods, bounded responses, and sanitized errors | A database, workspace/user authorization, browser/web framework routes, encryption-key management, notifications, provider-specific scopes, or tool policy |
| Host application | Workspace installation records, settings UI, manager permissions, callback route, single-use state bound to user/workspace/server, encrypted client/refresh/static credentials, provider presets, tool discovery/review, agent selection, refresh scheduling, reauthorization notifications, and run-to-installation bindings | Remote tool execution or storage of run credentials inside Runtime |
| Agent Runtime SDK client | Typed `mcp_servers` request objects, per-run credential rotation, auth-completed resume intent, and response/event models that do not expose credentials | Long-lived MCP installation or OAuth state |
| Agent Runtime service | Strict attachment validation, encrypted run credential, isolated MCP session/tools, approval/audit policy, 401/expiry authentication pause, credential replacement, and terminal cleanup | Browser OAuth, refresh tokens, workspace installation policy, or app notifications |

The OAuth helpers are headless by design: they return a browser URL, state, and
PKCE verifier. The app must durably store a hash of the state and encrypt the
verifier before redirecting. The callback must atomically consume that state,
verify the initiating user/workspace/server binding, exchange the code, and
store the refresh token in the app—not in Agent Runtime.

### Go app-side OAuth example

```go
import "github.com/helpin-ai/agent-runtime-go/mcpauth"

oauthClient, err := mcpauth.NewClient(
    installation.EndpointURL,
    // Include authorization hosts that are intentionally separate from the
    // MCP resource host. Prefer an app egress policy as an additional guard.
    mcpauth.WithAllowedHosts("login.provider.example"),
)
if err != nil { return err }

configuration, err := oauthClient.Discover(ctx)
if err != nil { return err }

registration, err := oauthClient.Register(
    ctx,
    configuration.Authorization.RegistrationEndpoint,
    callbackURL,
)
if err != nil { return err }

authorization, err := oauthClient.NewAuthorizationRequest(
    configuration,
    registration.ClientID,
    callbackURL,
    installation.Scopes,
)
if err != nil { return err }

// Atomically persist mcpauth.HashState(authorization.State) plus the encrypted
// verifier, workspace/user/server binding, expiry, client registration, and
// return path. Then redirect the user's browser to authorization.URL.
```

At callback, consume the state once and call `ExchangeCode`. Before a run or
credential rotation, call `Refresh` under an installation-level lock and store
any rotated refresh token before sending the returned access token to Runtime.

### Python app-side OAuth example

```python
from agent_runtime import MCPOAuthClient, hash_mcp_oauth_state

oauth_client = MCPOAuthClient(
    installation.endpoint_url,
    allowed_hosts=["login.provider.example"],
)
configuration = oauth_client.discover()
registration = oauth_client.register(
    configuration.authorization.registration_endpoint,
    callback_url,
)
authorization = oauth_client.new_authorization_request(
    configuration,
    registration.client_id,
    callback_url,
    installation.scopes,
)

# Persist hash_mcp_oauth_state(authorization.state), the encrypted verifier,
# user/workspace/server binding, expiry, and registration; then redirect to
# authorization.url.
```

The SDK URL allowlist covers discovered endpoints. Applications should also
enforce outbound DNS/network policy; Helpin's reference implementation adds
single-resolution public-IP dialing, no environment proxy, and provider
rollout controls.

## Reference application architecture

Helpin is the full reference application, not just a request example. Its
[`EXTERNAL_MCP_SERVERS.md`](https://github.com/helpin-ai/helpin/blob/develop/docs/EXTERNAL_MCP_SERVERS.md)
documents workspace tables, encrypted credential/state storage, browser OAuth,
Customer.io presets, notifications, tool review, agent catalog integration,
exact run bindings, credential rotation, and end-to-end tests. New applications
can replace Helpin's repository/UI/permission adapters while retaining the SDK
OAuth and Runtime wire contracts above.

During coordinated SDK/application development, consume an unpublished local
SDK with a Go workspace rather than committing a machine-specific `replace`:

```bash
go work init ./helpin/server ./agent-runtime-go
GOWORK="$PWD/go.work" go test ./helpin/server/...
```

Publish and pin the SDK before building the application without that workspace.

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

Connection, browser, proxy, and MCP protocol headers such as `Host`, `Cookie`,
`Proxy-Authorization`, `Content-Length`, `Mcp-Session-Id`,
`Mcp-Protocol-Version`, and `Origin` cannot be supplied as credential headers.

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

The SDK sends credentials only in start or credential-rotation requests.
`AgentRun`, list/search responses, events, messages, artifacts, tool-call
records, and rotation responses do not contain the credential.

## Rotating a paused run credential

If a connection returns HTTP 401 during MCP initialization or a later remote
tool call, or its declared `expires_at` has passed, Agent Runtime pauses the run with
`pause_reason=authentication` and creates an `authentication` interaction. The
interaction request identifies only the MCP `server_id`, `server_name`, and
reason.

The host app should refresh the workspace credential, send only the new access
token, then resume the same run with `intent=auth_completed`:

```http
PUT /v1/runs/{run_id}/mcp-servers/{server_id}/credential?app_id=helpin
Content-Type: application/json

{
  "credential": {
    "type": "bearer_token",
    "access_token": "replacement-short-lived-secret",
    "expires_at": "2026-07-30T20:00:00Z"
  }
}
```

Rotation is accepted only for an existing attachment on a non-terminal run.
It cannot change the server URL, transport, or tool allowlist. Updating a
credential does not replace headers in an already-open remote session; use it
before resume or before the next execution attempt.

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
the MCP handshake fails for a non-authentication reason, or an authorized tool
is not advertised by the server. Expired or HTTP-401 credentials pause the run
for host-managed credential rotation.

Relevant MCP references:

- [MCP authorization](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)
- [MCP security best practices](https://modelcontextprotocol.io/docs/tutorials/security/security_best_practices)
