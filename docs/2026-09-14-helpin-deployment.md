# Helpin credentials: fresh installation and staged upgrade

Runtime still supports its own provider environment keys for standalone execution
and other apps. Helpin opts into `require_run_model_credentials` in trusted app
configuration. The engine enforces this before queueing; no caller flag can weaken
it. Provider readiness is a startup snapshot, not an authentication probe. Restart
the API and all workers after changing keys or app configuration.

## Fresh host installation

Use the released Go module dependencies in `go.mod`; no private module replacement
is needed. Copy `.env.example` to a private environment file. Set:

- A dedicated Runtime Postgres database, reachable NATS and Temporal addresses.
  Helpin and Runtime share NATS but use their own database configuration.
- `AGENT_RUNTIME_SERVICE_TOKEN`, also configured in Helpin's runtime client.
- A stable `AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY`: generate once using
  `openssl rand -base64 32`. Install the SAME value on API and every worker. Do not
  regenerate on restart. Generate the independent MCP key in the same way.
- `HELPIN_INTERNAL_API_SECRET` equal to Helpin's `INTERNAL_API_SECRET`.
- An absolute `AGENT_RUNTIME_APP_CONFIG=@/path/to/apps.yaml`. Copy
  `ops/examples/helpin-apps.yaml`, replacing the Helpin URL/root directory as needed.
  The template supplies context/tools/skills/workspace and credential callbacks.
  Keep any additional authorization/browser restrictions appropriate to the app.

Helpin has its own separate stable `AI_CONNECTION_ENCRYPTION_KEY`, runtime URL,
runtime service token, NATS and Temporal configuration. Community is the default
Helpin build; SaaS requires `-tags ee` and frontend `VITE_EDITION=ee`.

Build from the runtime repository:

```bash
go build -o bin/agent-runtime ./cmd/agent-runtime
go build -o bin/agent-runtime-worker ./cmd/agent-runtime-worker
```

Load the private environment in each terminal/service and run each command as a
separate host process:

```bash
./bin/agent-runtime
./bin/agent-runtime-worker
AGENT_RUNTIME_WORKER_HEALTH_ADDR=127.0.0.1:8092 ./bin/agent-runtime-worker --coding
```

The coding worker is needed only for repository tools; supply the toolchain used
by your projects. API startup applies Runtime's own SQL schema migrations. Shared
durable state is required for restart recovery; the default memory compose is a
standalone smoke example and is not the Helpin deployment template.

## Linux compose alternative

`ops/examples/compose.helpin.yaml` runs these same binaries against existing host
Postgres/NATS/Temporal services using host networking. It does not start or restart
those services. Set the absolute `RUNTIME_ENV_FILE` and `RUNTIME_APP_CONFIG_FILE`,
and export the shared model encryption key for Compose interpolation. Validate
without printing expanded secrets:

```bash
docker compose -f ops/examples/compose.helpin.yaml config --quiet
docker compose -f ops/examples/compose.helpin.yaml up --build -d
# Add --profile coding before up when a coding worker is needed.
```

For non-Linux/container-network deployments, replace host networking and every
loopback URL with an address reachable from each container. The credential-refresh
callback requires HTTPS unless its hostname is `localhost` or `127.0.0.1`; do not
replace it with a plain-HTTP container service name. Persist the coding
workspace volume and give its directory permissions to the image's `node` user.

## Existing Helpin upgrade

1. Keep current app defaults while building released SDK/runtime/Helpin changes.
   Preserve all other apps' config and provider keys.
2. Apply Helpin's core migrations and (for SaaS) registered EE migrations. Run the
   Helpin `ai-bootstrap` preview, inspect the unconfigured routes and nonterminal
   legacy-run inventory, then explicitly apply it with chosen credential mappings.
3. Restart Helpin API, its worker and frontend into the SAME edition. Restart the
   Runtime API and all workers with matching encryption and callback configuration.
4. Test resolved API-key credentials and ChatGPT with Helpin profiles. Local
   endpoints require administrator bindings. Verify actual transport auth, tools,
   interruption/resume, and terminal cleanup. Complete expired-OAuth refresh and
   reconnect/revocation separately from ordinary inference.
5. Finish or explicitly cancel nonterminal Helpin runs that still depend on global
   credentials. Never rewrite their identity. Enable strict policy only after the
   credential canaries and drain have passed; restart API/all workers together.
6. With global keys present, confirm missing Helpin credentials are rejected before
   queueing while a separate app without strict policy retains defaults. A dedicated
   Helpin deployment can then omit provider keys entirely.

Do not enable SaaS BYOK merely to test migration. The EE operator must configure an
immutable flat token tariff and explicitly enable the workspace after acceptance.
Existing EE runs must drain before changing to Community: its lifecycle rejects an
EE pricing snapshot instead of silently discarding a previously accepted charge.
Historical SQL and the shared migration ledger remain intact in either edition.

September 14 validation: adapter fixtures and a real local Qwen tool/continuation
run passed. Fresh deployed Helpin/Runtime canaries, live worker restart, expired
ChatGPT refresh and revocation remain pending the operator's migration/restart.
