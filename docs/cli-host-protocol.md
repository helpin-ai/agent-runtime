# CLI host protocol: agent-runtime-cli/v1alpha1

This is the implemented experimental protocol. It is separate from the runtime's
trusted server-to-server API. A host authenticates the human and authorizes every
operation; local CLI status, identity fields, usage reports, and tool output are
untrusted client input. Do not expose runtime service credentials or provider
keys to the CLI. Version changes must be explicit.

## Discovery and authentication

`GET /agent-runtime/cli.json` at the URL passed to `connect` returns:

```json
{
  "protocol_version": "agent-runtime-cli/v1alpha1",
  "app_id": "your-app",
  "name": "Your App",
  "api_base_url": "https://app.example/cli/v1",
  "issuer": "https://auth.example",
  "client_id": "registered-public-cli",
  "resource": "https://app.example/cli/v1",
  "scopes": ["agent:local"],
  "capabilities": ["admission", "execution_leases"]
}
```

Capabilities are explicit for new hosts: `admission`, `execution_leases`, and
`model_gateway`, `event_sync`, and `artifacts` describe independently available surfaces. The CLI refuses a
connected `run` before admission when `model_gateway` is absent, and directs the
user to `admit`. An omitted capability list retains legacy alpha fixture behavior;
new hosts must send a list, including an empty list when no feature is available.

All endpoints require HTTPS; literal loopback IPs may use HTTP for development.
Discovery/API requests reject redirects. The descriptor pins the issuer, public
client, API base, and resource for the named connection; reconnect using a new
name to change them.

The issuer exposes RFC 8414 authorization-server metadata with an exact matching
`issuer`, `authorization_endpoint`, `token_endpoint`, and
`code_challenge_methods_supported` containing `S256`. Register the public client
for browser authorization code + PKCE and loopback redirects of the form
`http://127.0.0.1:{ephemeral-port}/callback`. No client secret is used. The CLI sends
`resource` in authorization, code exchange, and refresh requests. Access tokens
must be Bearer tokens. Standard `expires_in` and optional `refresh_token` are
supported. An optional `revocation_endpoint` accepts form POST `token` and
`client_id`. Logout revokes the refresh token (or access token if no refresh is
stored) before removing local credentials. Failed revocation retains credentials
so the operation can be retried. Legacy issuers without a revocation endpoint
retain local-only logout. Browser cancellation returns `error=access_denied` with
the original state and never stores credentials.

The host enforces audience, scopes, account/workspace membership,
consent, budgets, and revocation.

## Authenticated resources

All paths below are relative to `api_base_url`; all require the user Bearer token.
JSON errors need an appropriate non-2xx status; the CLI deliberately does not
echo arbitrary error bodies that might contain credentials.

- `GET /me`: user/account display data, rendered as JSON.
- `GET /agents`: the host's allowed agent catalog, rendered as JSON.
- `POST /runs`: authorize and admit a **local** execution, without starting a
  remote worker.
- `POST /runs/{run_id}/model`: generate one model response under host policy.
- `POST /runs/{run_id}/events`: ingest idempotent local execution history.

### Admission

Request:

```json
{
  "request_id": "cli_request_...",
  "agent_id": "coder",
  "target": "task:123",
  "instructions": "Implement the requested change",
  "execution_location": "local",
  "review": false
}
```

`target` is an opaque string for the host to resolve; it need not be a task.
`request_id` is a client idempotency key. Hosts should deduplicate requests by
connection + request_id and reject a different payload with HTTP 409. The `admit`
command persists the request before sending it; repeating `--request-id` and the
same arguments recovers the same admission. A conflicting local request is refused
before any HTTP call. Requests are saved beneath `admissions/CONNECTION/`.
An ambiguous server-side preparation failure must never cause an automatic second
launch or usage reservation; Helpin returns 409 if a reserved request has no run.
After inspecting the failure, a user can choose a new request ID.

Response:

```json
{
  "run_id": "host_run_123",
  "agent": {
    "id": "coder",
    "name": "Coding agent",
    "system_prompt": "Your approved instructions",
    "approval_mode": "mutating_tools"
  },
  "context": "Authorized target context",
  "allowed_tools": ["read_files", "list_directory", "repository_search", "write_file", "edit_file", "apply_patch", "run_command", "request_user_input", "request_approval"]
}
```

The CLI copies prompt/name/approval policy into a local agent snapshot. Its local
workspace path comes exclusively from the local user's `--dir`. Remote workspace
configuration, provider credentials, runtime adapters, executable commands, and
arbitrary remote tool registrations are not imported from the agent response.
The tool list is intersected with the local coding/review set; unexpected model
tool calls are rejected at execution time. Host approval requirements cannot be
weakened by `--yes`; a host agent with `workspace.access=read_only` also constrains
the local workspace access mode. Hosts must reject unsupported policy requirements rather
than assume the client enforces an unspecified capability.

### Execution grants (`execution_leases`)

Admission adds `execution`:

```json
{
  "id": "execution_123",
  "run_id": "host_run_123",
  "epoch": 1,
  "local_run_id": "",
  "policy_hash": "sha256-of-immutable-admission",
  "lease_expires_at": "2026-09-15T15:15:00Z"
}
```

A grant identity is not a bearer credential. Every operation also requires the
connection's current user token and current target/agent authorization.

| Method/path | Input | Result |
| --- | --- | --- |
| GET `/runs/{id}/execution` | — | Current grant, including an expired lease |
| POST `/runs/{id}/bind` | `epoch`, `local_run_id` | Bound grant; 409 if expired, stale, or already bound elsewhere |
| POST `/runs/{id}/renew` | `epoch` | Renewed grant; 409 on a stale or racing update |
| POST `/runs/{id}/revoke` | `{}` | 204; repeatable cancellation of the local admission |

Helpin leases last 15 minutes. Renewing a live lease preserves its binding and
epoch; renewing an expired lease increments the epoch and clears the binding.
Old clients cannot bind or renew the new epoch. Revocation prevents renewal and
cancels the normal undispatched run, releasing its usage reservation. Revoking
the OAuth connection also denies all derived grant operations. Local run IDs and
client completion claims never authorize cloud worker events or billing.

The CLI also binds automatically before execution and renews every 30 seconds.
On resume it fetches the current grant and renews an expired lease before binding.
Model and result requests carry the bound local run ID and current epoch.

### Model generation

For hosts without leases, the request is `runtime.NativeModelRequest`:

```json
{
  "system_prompt": "...",
  "messages": [{"role": "user", "content": "..."}],
  "tools": [{"name": "read_files", "description": "...", "input_schema": {}, "mutating": false}],
  "step": 0
}
```

For `execution_leases` hosts, wrap that request in:

```json
{"request_id":"stable-sha256", "epoch":1, "local_run_id":"run_local", "request":{}}
```

`request` contains the complete native request above. The client hashes the local
run ID and native request to make retries stable. Helpin rejects changed payloads
for the same ID, journals provider responses, and returns saved responses on replay.
An uncertain dispatched attempt blocks new attempts; it is never redispatched
automatically. The host owns the system prompt and resolves the admitted AI profile
again for each generation. Helpin initially supports OpenRouter API-key profiles.

Responses use `runtime.NativeModelResponse`:

```json
{
  "message": {
    "role": "assistant",
    "blocks": [{"type": "tool_call", "tool_call_id": "call_1", "tool_name": "read_files", "input": {"files": [{"path": "main.go"}]}}]
  },
  "usage": {"input_tokens": 120, "output_tokens": 40}
}
```

A text answer can use `message.content`. Native messages may contain text,
tool_call, and tool_result blocks and provider continuation data. See the Go
structs for optional fields. This initial gateway is JSON request/response, with
a 140-second model HTTP timeout and an 8 MiB response decoding bound. Streaming transport
and a standalone public protocol SDK remain follow-ups.

The host resolves its own provider/model and attaches its own credentials. It
must validate ownership of the admitted run, current user access, tool definitions,
budget, and other policy on every request. Bill from host-observed model usage,
not CLI event claims. A modified CLI is not a trusted execution environment.

### Event sync

The CLI posts:

```json
{
  "local_run_id": "run_...",
  "status": "completed",
  "events": [],
  "messages": [],
  "output_summary": {}
}
```

Lease hosts also require `epoch`. Messages may include `content_blocks` and
`tool_invocations`; client token usage is never accepted as billing evidence.

Events use Agent Runtime event envelopes, including stable `event_id` and local
sequence numbers; messages include stable IDs. The host must deduplicate by the
authenticated host run plus event/message ID. Retries replay saved history rather
than invent new IDs. Do not authorize from the payload's local `app_id`/`run_id`.
Return any 2xx status after accepting the batch. The initial client resends full
history, so hosts should set appropriate request limits; paginated incremental
sync is planned. Model/token deltas are live UI traffic and are not all persisted.
Local command results and substantive lifecycle events are persisted.

### Artifacts (`artifacts`)

`POST /runs/{id}/artifacts` accepts `epoch`, `local_run_id`, stable `request_id`,
`kind` (`patch`, `test_log`, or `report`), and UTF-8 `content` up to 256 KiB.
Helpin stores private inline artifacts with explicit local-report provenance.
The CLI uploads tracked unstaged Git patches automatically after result sync;
event history is also saved as a separate JSON artifact. No cloud completion or
trusted verification is inferred from these reports.

The CLI installs no hosted endpoints. Helpin's gated adapter implements discovery,
OAuth, admission, execution leases, model generation, and shared local results. The independent host fixtures are
`internal/cli/connection_test.go` and `internal/cli/admission_test.go`; Helpin tests
the same binary against its handlers in `cli_http_integration_test.go`. Helpin's
connected tests cover real local edits/tests, usage settlement, and replay.
