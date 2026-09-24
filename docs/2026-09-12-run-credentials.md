# Optional app-owned model credentials

Apps continue to use the runtime's provider keys by default. An app can opt into `require_run_model_credentials: true` to require host-supplied model and credentials at engine admission; Usermaven and standalone defaults remain supported. A backend can optionally send `model` and `model_credential` on `POST /v1/runs`. An explicitly supplied credential never falls back to the runtime key. The selected model is pinned in the run input; secrets are excluded from that input, events, checkpoints, tool environments, and public run responses.

```json
{
  "app_id": "helpin",
  "agent_id": "agent-id",
  "target": {"type": "workspace", "id": "workspace-id"},
  "model": {"provider": "openai", "model": "gpt-5.6-luna"},
  "model_credential": {
    "type": "api_key",
    "api_key": "<backend-supplied-key>",
    "connection_id": "<app-connection-id>"
  }
}
```

The app owns permanent API keys, device sessions, OAuth access/refresh tokens, user authorization, and refresh serialization. The SDK implements the device protocol and refresh exchange. Agent Runtime owns only an encrypted, per-run API key or access token. It has no ChatGPT connection table, refresh token, device-login route, Codex process, or Codex home directory.

## Configuration and rollout

1. Use released Go SDK `v0.7.0` (Helpin and Runtime are pinned). No local workspace substitution or unpublished revision is required. Usermaven needs no change for existing default-key runs.
2. Configure the same `AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY` on the runtime API and every worker. It accepts 32 raw bytes or base64 encoding of 32 bytes. Keep it stable across restarts. Existing installations without it retain their default-key behavior.
3. Apply Helpin migration `202609120002_personal_ai_connections.sql`. Configure a separate `AI_CONNECTION_ENCRYPTION_KEY` in Helpin. Personal connections become available when encryption and the runtime client are configured. The frontend never receives a provider API key, access token, or refresh token.
4. Configure the trusted callback in the runtime app configuration. The URL is deployment configuration, never run input. `HELPIN_INTERNAL_API_SECRET` below must contain Helpin's `INTERNAL_API_SECRET`, not its runtime-client service token.

```json
{
  "app_id": "helpin",
  "model_credential_callback": {
    "url": "https://helpin.example/api/internal/agent-runtime/model-credentials/refresh",
    "token_env": "HELPIN_INTERNAL_API_SECRET"
  }
}
```

5. Leave `AGENT_RUNTIME_CHATGPT_ENABLED` and Helpin's `CHATGPT_CONNECTIONS_ENABLED` unset/false until the live subscription gate below passes. API-key connections do not depend on subscription enablement. `CHATGPT_OAUTH_CLIENT_ID` is optional and configured by the app; the SDK includes Codex's public client ID as its protocol default.

`GET /v1/capabilities` reports `auth_modes` and `run_credentials_configured` for each provider. `configured` continues to mean a global API key is present. ChatGPT has no global key path.

## Refresh, resume, and disconnect

Before each model request, including context summaries, the runtime reads the current encrypted credential. It asks the trusted app callback for a replacement near expiry or after an initial HTTP 401. It retries that authentication rejection once, before a response stream starts. It never retries a partially consumed stream or replays tools as part of refresh.

The callback receives app/run/host-run/connection/provider/account identity, reason, and a SHA-256 fingerprint of the rejected credential. Helpin verifies the active run's owner and connection, active workspace membership, runtime mapping, and account identity. A database row lock serializes device polls and refresh-token rotation across replicas. The fingerprint prevents concurrent 401 callbacks from rotating an already-refreshed token again. Runtime replacement also uses compare-and-swap and rejects connection/account changes.

An unavailable/revoked credential pauses the run with an authentication interaction. Reconnect updates affected run credentials and resumes only matching model-authentication interactions. Other approval and MCP-authentication pauses stay pending. Disconnect erases the app secret and revokes credentials on bound active runs; failed revocations can be retried. Launch rechecks the connection after recording its runtime binding to cover disconnects racing admission. Already-sent model requests can finish; revocation applies to subsequent requests. Paused runs retain their encrypted run credential; terminal cleanup clears it and prevents resurrection by a delayed refresh.

Helpin connections are personal to a user within a workspace. Teammates cannot use or reconnect them for that user's runs. Scheduled root runs cannot use personal credentials. Descendants retain the originating owner's selected connection/model, and changing either requires a new independent run/chat. Global runtime-key launches remain unchanged.

The original full-rate Helpin connection contract is superseded by AI profiles: Community has no Helpin model/tool charges; opted-in SaaS BYOK uses a versioned flat fee per million normalized tokens with paid tools billed separately. These policies belong to Helpin, not Runtime. Historical `customer_funded_platform` and ten-percent records remain readable.

## Subscription provider boundary

`runtime_kind=native_sdk`, `provider=openai_chatgpt` uses the Responses serializer/parser against the subscription endpoint. It sends account routing, instructions, streaming requests, `store:false`, and encrypted reasoning inclusion. It omits API-only output-token and previous-response fields. Summaries also stream without tools. A missing completion event fails closed before tool execution. Opaque reasoning signatures and item metadata survive internal checkpoints; public messages omit them. Changing providers with such a checkpoint is rejected.

Protocol references: [Codex device authentication](https://learn.chatgpt.com/docs/auth), [externally managed app-server tokens](https://learn.chatgpt.com/docs/app-server#3c-log-in-with-externally-managed-chatgpt-tokens-chatgptauthtokens), and [Codex source revision](https://github.com/openai/codex/tree/53c542d944c705f3a66780a19223223bee57cbb6). The SDKs include source attribution and Apache-2.0 license notices. Those references do not establish a generally supported third-party hosted subscription API. This provider remains an opt-in integration subject to live account and deployment validation.

## Verification and release gate

- Go and Python SDK suites passed (41 Python tests); the Python wheel and source distribution include the authentication module and license notices. Affected runtime/Helpin backend suites and Go vet passed using pinned dependencies with `GOWORK=off`. Credential manager/store race tests passed.
- Runtime regression tests cover encrypted SQL persistence/restart, atomic admission, concurrent version changes, app isolation, revocation, authentication pauses, terminal cleanup, account pinning, real Eino request serialization, streamed summaries, truncated streams, and provider-state checkpoint replay.
- Helpin tests cover owner/workspace isolation, revoked membership, encrypted storage, key replacement, concurrent refresh deduplication, manual-run restrictions, and full platform charges. Frontend type checks and the production build passed for all three launch surfaces and authentication recovery controls. The build retains existing bundle-size and Lottie eval warnings. Isolated Playwright checks passed for selection, credit disclosure, API-key clearing after save, device polling, keyboard dismissal, desktop layout, and mobile dark mode; 29 existing chat/run-view tests passed.
- The opt-in real OpenAI test `TestNativeRunCredentialLiveSmoke` passed on 2026-09-12 with `gpt-5.6-terra`, global `OPENAI_API_KEY` unset, and the credential supplied through the run store: generation 7 output tokens; summary 18 output tokens. Output: `/tmp/native-run-credential-smoke.log` in the development workspace.
- The SDK successfully initiated a real device-login request. Automatic approval review blocked the subsequent consent/inference/refresh test because live subscription validation requires separate approval. Live ChatGPT device consent, account eligibility, inference/tool continuation, and reconnect after token expiry remain a release gate. Mock protocol success is not evidence that an account is eligible. Keep both subscription flags false until these checks pass in the intended deployment.
- Production deployment, the earlier retirement inventory, and disposition of historical Codex runs are not performed by this change. Follow the native cutover runbook before any production cutover.


### September 13 test-system follow-up

The later investigation of runtime run `run_9b1445790f13e52c710f2c3d` (Helpin run
`086a992f-a4ca-479b-8923-67923861fa9d`) observed `openai_chatgpt` / `gpt-5.6-terra`
using an app-owned OAuth credential, with 10 successful model responses and 37 tool
calls. The user reconfirmed this test on September 14. The run then stalled in a
Git tool; this establishes inference/tool execution, not full-run completion or
expired-token refresh. It supersedes the inference portion of the September 12
blocked test record above. Refresh, reconnect, and revocation remain separate live
release gates. ChatGPT removes `previous_response_id`; ordinary tool continuation
must not be described as lossless provider-state replay.


### Run model controls

SDK `v0.6.0-alpha.1` adds optional `model.controls` for reasoning effort, service
tier, and OpenRouter provider preferences. Omitted controls retain legacy agent
controls. An explicit object replaces all legacy model controls, including `{}`;
it does not replace tool limits or native-context settings. Engine admission uses
the SDK validator, and the model factory applies the same validator before
execution. Provider capability `lossless_response_replay` is false for ChatGPT.

App capability summaries include a `model_credentials` component when an app has
a refresh callback. `configured` and `auth_configured` describe the loaded app
configuration; neither establishes a successful refresh. Callback tokens and URLs
are omitted from this component. Helpin uses it together with the provider's
`run_credentials_configured` and `auth_modes` before admitting a ChatGPT route.
Global `configured` provider-key availability remains independent and retains its
startup-snapshot semantics for standalone and other host applications.

Fresh host and Linux compose configuration: [Helpin deployment](2026-09-14-helpin-deployment.md). Approved compatible routes: [Chat Completions endpoints](2026-09-14-compatible-models.md).
