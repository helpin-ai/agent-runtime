# Configure Agent Runtime

Use this reference to choose a worker image and configure an existing runtime
installation. Run build commands from the repository root. For your first local run, start with the [quickstart](quickstart.md).

## Runtime image toolchain


The default `support` image includes the non-root runtime binaries, Node, Git,
search tools, and browser dependencies. It excludes compilers and both retired
coding engines. The worker binary is the same in every image; the default image
lacks the compilers that repository builds need, so serve execution from the
`coding` image.

The separate `coding` target includes Go, Python, Rust, Make, and Node package
tools for trusted repository workloads. Run its worker with `--coding`; it
polls only `agent-native-coding`, with up to four concurrent activities per process. Normal workers poll native
interactive, autonomous, and automation queues. `--all-queues` serves every queue
from one process for single-tenant installs. Admission derives coding requirements from
effective shell/write permissions and rejects them without a coding poller.

```bash
docker build -t agent-runtime:local .
bash scripts/container-default-smoke.sh agent-runtime:local
docker build --target coding -t agent-runtime-coding:local .
bash scripts/container-toolchain-smoke.sh agent-runtime-coding:local
```

Enable `codingWorker.enabled` in Helm only for a dedicated trusted deployment.
It uses one worker and a retained workspace PVC. See the
[native cutover runbook](native-cutover.md) before upgrading an existing
installation. Native database checkpoints preserve the conversation, not files;
continuation stops if its previous repository workspace is unavailable.

## Persistent storage and workers

Choose `sqlite` or `postgres` to retain run data. Set `TEMPORAL_ADDRESS` on the
API to use durable execution and start a worker from the same checkout:

```sh
TEMPORAL_ADDRESS=localhost:7233 go run ./cmd/agent-runtime-worker
```

API and workers must share their database, app configuration, and relevant
credentials. For a configured Helpin deployment, use the
[Helpin deployment guide](2026-09-14-helpin-deployment.md). For trusted execution
and Python analysis, see [opt-in execution](2026-09-17-opt-in-execution.md).
The [compatible model guide](2026-09-14-compatible-models.md) covers approved
local Chat Completions endpoints.

## Environment variables

- `AGENT_RUNTIME_SQLITE_DSN`: SQLite database path when using `sqlite`
- `AGENT_RUNTIME_NATIVE_PROVIDER`: provider selection for standalone runs
- `AGENT_RUNTIME_STORE_DRIVER`: `memory`, `sqlite`, or `postgres`
- `DATABASE_URL`: Postgres DSN when using Postgres
- `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`, `PGSSLMODE`:
  used to build the Postgres DSN when `AGENT_RUNTIME_STORE_DRIVER=postgres`
  and `DATABASE_URL` is not set
- `AGENT_RUNTIME_SERVICE_TOKEN`: required bearer token for `/v1` and legacy
  `/internal` service APIs
- `AGENT_RUNTIME_ALLOW_ANONYMOUS`: local-development escape hatch. Set to
  `true` only for isolated local runs without `AGENT_RUNTIME_SERVICE_TOKEN`.
- `AGENT_RUNTIME_APP_CONFIG`: JSON app adapter/MCP config, or `@/path/file.json`
  (also supports app-scoped, event-type-filtered HTTP callbacks)
- `AGENT_RUNTIME_MCP_CREDENTIAL_ENCRYPTION_KEY`: shared API/worker key used to
  encrypt credentials attached to individual runs; required when credentials
  are supplied
- `AGENT_RUNTIME_MCP_ALLOWED_HOSTS`: optional comma-separated host allowlist
  for run-scoped MCP URLs
- `AGENT_RUNTIME_MCP_ALLOW_PRIVATE_NETWORKS`: opt in to trusted private-network
  MCP destinations; denied by default
- `AGENT_RUNTIME_MCP_ALLOW_HTTP`: local-development-only HTTP opt-in for
  run-scoped MCP; HTTPS is required by default
- `ANTHROPIC_API_KEY`: enables Eino-backed Anthropic `native_sdk` execution
- `OPENAI_API_KEY`: enables Eino-backed OpenAI Responses `native_sdk` execution
- `OPENROUTER_API_KEY`: enables Eino-backed OpenRouter Responses `native_sdk` execution
- `TINYFISH_API_KEY`: enables the preferred provider for compatible fast
  `web_search` calls in both the API and durable worker processes. TinyFish
  Search is free within its provider rate limit (currently 30 requests/minute
  per key); rate limits and provider failures fall back to Exa when available.
- `EXA_API_KEY`: enables the first fallback for ordinary `web_search` calls and
  the required provider for deep search, extraction, freshness/date filters,
  categories, and synthesized output. Enabling the tool on an agent grants
  permission but does not supply provider credentials.
- `BRAVE_SEARCH_API_KEY` or `BRAVE_API_KEY`: enables the final fallback for
  compatible fast-search calls. Requests only downgrade to providers that can
  preserve their requested filters.
- `WEB_FETCH_PROXY_URLS`: optional comma/newline-separated proxy URLs for
  `fetch_url` and `crawl_url`
- `AGENT_RUNTIME_BROWSER_ENABLED`: enables shared browser infrastructure when
  set to `true`. Each host app must also opt in through its
  `AGENT_RUNTIME_APP_CONFIG` `browser` block. Local Chromium is used when
  `KERNEL_API_KEY` is absent or Kernel rejects session creation because billing
  credit is unavailable.
- `AGENT_RUNTIME_BROWSER_CHROMIUM_EXECUTABLE`: optional local Chromium/Chrome
  executable override. The default and coding container images include
  `/usr/bin/chromium`; local installations may rely on agent-browser discovery.
- `AGENT_RUNTIME_BROWSER_CHROMIUM_ARGS`: optional comma-separated Chromium
  launch arguments used only by the local backend. Hardened Kubernetes pods
  that already provide the container isolation boundary should set
  `--no-sandbox,--disable-dev-shm-usage`; these arguments are never applied to
  Kernel CDP sessions.
- `AGENT_RUNTIME_BROWSER_SESSION_TIMEOUT_SECONDS`: browser session timeout,
  default `300`; the runtime closes sessions when runs become terminal and the
  idle timeout cleans up sessions retained across paused conversation turns
- `KERNEL_HEADLESS`: defaults to `true` so normal navigation, interaction, and
  screenshots use Kernel's lower-cost headless browsers. Starting
  `browser_record` on Kernel securely transfers cookies and web storage to a
  replacement headful browser for that run because Kernel replays require a
  GUI. Local Chromium records the existing run session directly. Set this to
  `false` only as an operational override for workloads that require headful
  stealth or live-view behavior from the first page.
- `AGENT_RUNTIME_BROWSER_MAX_OUTPUT_CHARS`: maximum compact snapshot output,
  default `8000`
- `AGENT_RUNTIME_BROWSER_REPLAY_FRAMERATE`: Kernel replay frame rate used by
  `browser_record`, default `15` and bounded to `1`-`20`
- `AGENT_RUNTIME_BROWSER_RECORDING_SMART_TRIM_ENABLED`: automatically removes
  idle agent-reasoning gaps from recordings while retaining browser actions
  and page loads; defaults to `true`
- `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_TIMEOUT_SECONDS`: FFmpeg processing
  timeout, default `90` and bounded to `10`-`300`
- `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_THREADS`: FFmpeg video encoder threads
  per recording, default `1` and bounded to `1`-`4`
- `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_MAX_CONCURRENT`: maximum simultaneous
  FFmpeg jobs per Agent Runtime process, default `1` and bounded to `1`-`4`
- `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_PRE_PADDING_MS` and
  `AGENT_RUNTIME_BROWSER_RECORDING_TRIM_POST_PADDING_MS`: context retained
  around each browser operation, default `750` and `1500` milliseconds
- `AGENT_RUNTIME_BROWSER_FFMPEG_BINARY`: FFmpeg binary override; the runtime
  image includes `ffmpeg` and uses it by default
- `KERNEL_BASE_URL`: optional Kernel API base URL override for self-hosted or
  development environments
- `KERNEL_API_KEY`: optional Kernel cloud-browser credential. When configured,
  Kernel is preferred; local Chromium remains the automatic fallback for
  unavailable billing credit. Browser recording works on either backend, but
  local Chromium recordings do not support `record_audio=true`.

Browser domains and private artifact upload credentials are app-scoped under
`AGENT_RUNTIME_APP_CONFIG`; see [app configuration](app-configuration.md). Browser sessions
are run-isolated and ephemeral. Kernel sessions are attached to agent-browser
over CDP and deleted at run cleanup; local sessions launch the packaged
Chromium process and close it through the same cleanup path. Local recordings
are captured as WebM, converted to H.264 MP4 with FFmpeg, and uploaded through
the same private artifact contract as Kernel recordings.
- `AGENT_RUNTIME_NATIVE_MODEL`: optional native SDK model override; defaults
  by provider are `claude-opus-4-8` for Anthropic, `gpt-5.6-terra` for OpenAI,
  and `openai/gpt-5.6-terra` for OpenRouter
- `TEMPORAL_ADDRESS`: enables durable Temporal execution
- `TEMPORAL_NAMESPACE`: Temporal namespace, defaults to `default`
