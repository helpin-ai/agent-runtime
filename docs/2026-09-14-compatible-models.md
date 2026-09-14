# Approved Chat Completions endpoints

SDK `v0.6.0-alpha.2` and the `openai_compatible` provider support Chat Completions
separately from OpenAI/OpenRouter Responses. Runtime environment defaults for
existing providers remain available to apps that do not require run credentials.
Compatible routes always require an approved endpoint and explicit run credential.

Configure a trusted app binding on the API and all workers:

```yaml
apps:
  - app_id: helpin
    # Include this app's normal authorization, callbacks, and tool configuration.
    require_run_model_credentials: true
    model_endpoints:
      - id: local-qwen
        base_url: http://127.0.0.1:8181/v1
        auth_mode: none
        allow_http: true
```

Use `auth_mode: api_key` for authenticated endpoints. HTTP requires administrator
opt-in; HTTPS does not. URLs must be canonical, without credentials, query,
fragment, or a trailing slash. App capabilities expose approved bindings, allowing
Helpin to accept an endpoint ID instead of a browser-provided URL.

A resolved run supplies:

```json
{
  "model": {
    "provider": "openai_compatible",
    "model": "local-qwen",
    "endpoint": {"id":"local-qwen","base_url":"http://127.0.0.1:8181/v1","auth_mode":"none"},
    "controls": {}
  },
  "model_credential": {"type":"none","connection_id":"host-owned-connection"}
}
```

Even no-auth runs have an encrypted, revocable credential record. The engine
checks the complete endpoint binding before admission and recovery. Requests can
only reach that binding's `/chat/completions` path; redirects are not followed.
No global provider key is inherited. Changing an approved binding invalidates old
selections rather than redirecting their credentials. Coordinate API/worker
configuration and recreate affected connections/profiles after a binding change.

Capabilities: streaming text/tools, interleaved tool arguments with stable IDs,
tool results, transcript continuation, reported usage, cancellation, and summaries.
There is no lossless Responses-chain replay, reasoning-effort/service-tier control,
or native Ollama protocol. Model context/output limits must fit the chosen server.

## Validation record — September 14, 2026

- Local server: official llama.cpp CPU release `b10955`, binary version
  `0.4.0-dev`, commit `2f539596c`, listening only on loopback.
- Model: `Qwen/Qwen3-0.6B-GGUF`, `Qwen3-0.6B-Q8_0.gguf`, revision
  `23749fefcc72300e3a2ad315e1317431b06b590a`; SHA-256
  `9465e63a22add5354d9bb4b99e90117043c7124007664907259bd16d043bb031`.
- Configuration: 8,192 context, Jinja template, thinking disabled, explicit no-auth.
- `TestCompatibleLocalLiveToolAndRestoredTranscript` passed: one tool call, a
  serialized/restored transcript and new provider instance, a final answer using
  the tool result; 404 input and 28 output tokens reported across both calls.
- This is actual local inference. It does not establish a Helpin browser canary
  or a live worker restart. Engine admission/recovery and credential lifecycle
  remain covered by regression tests; deployment canaries are separate gates.
- Protocol fixtures additionally cover two interleaved calls, cache/reasoning
  token subsets, summaries, truncation, cancellation, redirects, and revocation.

Reproduce with a local tool-capable server already running:

```bash
NATIVE_COMPATIBLE_SMOKE_BASE_URL=http://127.0.0.1:8181/v1 \
NATIVE_COMPATIBLE_SMOKE_MODEL=local-qwen \
go test ./internal/runtime -run '^TestCompatibleLocalLive' -v -count=1
```
