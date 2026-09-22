# Run your first article review

Start Agent Runtime locally, supply an article from a tiny host application, and
read proposed edits with the Python SDK. The host exposes only that article. No
publishing, shell, or repository tools are configured for the review agent.

## Before you start

You need access to this repository, Git, the Go version required by
[go.mod](../go.mod), Python 3.10+ with `venv` and `pip`, and an Anthropic API key.
The model request uses your provider account and may incur charges. Other
providers are supported; see [runtime configuration](runtime-configuration.md).

This example uses one local runtime process and an in-memory store. It does not
need Temporal, NATS, or a database. Restarting the runtime clears its agents,
runs, messages, and events. It is an evaluation setup, not a production deployment.

## 1. Prepare the checkout

Clone the repository if you do not already have it, then run the remaining
commands from its root:

```sh
git clone https://github.com/helpin-ai/agent-runtime.git
cd agent-runtime
python3 -m venv .venv
. .venv/bin/activate
python -m pip install "agent-runtime @ git+https://github.com/helpin-ai/agent-runtime-python.git@v0.5.0"
go build -o bin/agent-runtime ./cmd/agent-runtime
```

Use a checkout and its matching documentation together. The client example uses
Python SDK v0.5.0. For deployments, pin the runtime release and client version you
have validated together; see [CI and releases](ci-cd.md).

## 2. Start the example host

In the first terminal, from the repository root:

```sh
export EXAMPLE_CONTEXT_TOKEN=local-article-context-only
python3 examples/article-review/context_server.py
```

Leave this running. It prints `Sample article context: http://127.0.0.1:8092/context`.
The fixture returns one fictional export guide for `readme_demo` and rejects
other app IDs, targets, and tokens. A real application must authorize context
against its own users and records. Put the text the agent needs in the context
response’s `summary`; the built-in `get_context` tool reads that summary.

The two fixed tokens in this guide are for loopback-only evaluation. Use separate,
private credentials when connecting a real application.

## 3. Start Agent Runtime

In a second terminal, from the same checkout, make your `ANTHROPIC_API_KEY`
available through your shell or secret manager. Do not put the key in a committed
file. Then run:

```sh
export AGENT_RUNTIME_SERVICE_TOKEN=local-article-runtime-only
export EXAMPLE_CONTEXT_TOKEN=local-article-context-only
export AGENT_RUNTIME_ADDR=127.0.0.1:8090
export AGENT_RUNTIME_STORE_DRIVER=memory
export AGENT_RUNTIME_APP_CONFIG=@examples/article-review/apps.json
export AGENT_RUNTIME_NATIVE_PROVIDER=anthropic
# Use this example's local execution path, even if your shell has worker settings.
unset TEMPORAL_ADDRESS AGENT_RUNTIME_NATIVE_MODEL
export AGENT_RUNTIME_EVENT_SINK=log
: "${ANTHROPIC_API_KEY:?Set your Anthropic API key before starting the runtime}"
./bin/agent-runtime
```

Leave this terminal running too. The service reads the host configuration from
[apps.json](../examples/article-review/apps.json) and requests the article from
that host when the run starts. It uses the runtime's default Anthropic model;
set `AGENT_RUNTIME_NATIVE_MODEL` explicitly if your account requires another
supported model.

## 4. Check the connection and start a run

In a third terminal, from the repository root:

```sh
. .venv/bin/activate
export AGENT_RUNTIME_SERVICE_TOKEN=local-article-runtime-only
curl --fail http://127.0.0.1:8090/healthz
python examples/article-review/review.py
```

Health returns `{"status":"ok"}`. The client registers `article_reviewer`, starts
a run for `article/export-guide`, and prints event names, the final status, and
the proposed edits. A successful run ends with:

```text
Run started: <generated run ID>
... execution events ...
... run.completed
Run status: completed
... proposed edits ...
```

Event counts and model wording vary. The article remains unchanged: the host has
no write endpoint, and the agent has no publishing tool. The event stream is
long-lived, so the example stops reading on completion, failure, cancellation,
or pause rather than waiting for the server to close it.

Stop the runtime and the fixture with **Ctrl+C** in their terminals when finished.

## If something goes wrong

| Result | Check |
| --- | --- |
| Cannot clone the repository | Confirm repository access before using the source quickstart. |
| Python cannot create a virtual environment | Install your OS's Python venv support, then repeat step 1. |
| Connection refused | Keep both services running and check that ports 8090 and 8092 are free. |
| HTTP 401 from the runtime | The client and runtime must use the same service token. |
| Context request fails | Start the example host and match `EXAMPLE_CONTEXT_TOKEN` in both terminals. |
| Model factory is not configured or provider rejects the request | Check the runtime's provider key, account access, and model selection. |
| Agent or run disappears | The example store is in memory; restart the client after a service restart. |
| Run pauses or fails | Inspect its status and events. The script exits with an error instead of reporting success. |

## Connect your application next

Replace the fixture with your own [host context endpoint](interfaces.md), enforce
record access in your backend, and register the agent definitions your product
needs. Add [selected MCP tools](run-scoped-mcp.md) only when the workflow needs
them. Your application decides how proposals are reviewed and applied.

For persistent execution, choose storage and workers in
[runtime configuration](runtime-configuration.md). Keep the service API and its
credentials behind your backend; do not expose the service token to browsers.
