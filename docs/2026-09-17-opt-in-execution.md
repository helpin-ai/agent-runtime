# Opt-in execution, Python analysis and optional approval review

This is the bounded replacement for the [sandbox-platform proposal](plans/2026-09-17-unified-agent-sandbox-execution.md). Sandbox managers, provisioned pods, proxies and stronger isolation remain deferred proposals; they were not existing mechanisms removed by this change.

## Worker roles and packaging

The worker binary supports both roles. Default startup polls interactive, autonomous and automation queues. `--coding` polls the existing execution queue only. Shared workers still reject command-capable runs; execution admission still requires a live execution poller. Concurrency limits are **per process**, so shared queue throughput scales with replicas.

Production shared workers remain two replicas with Kubernetes rolling updates and no execution volume. Execution remains one replica, concurrency one, `Recreate`, with the existing retained RWO volume and serialized handoff. Update execution independently of shared workers. Render the production overlay and operator Helm values before deployment; chart defaults are not production settings.

Image digests need not match. EE shared workers retain the default Node/browser/ffmpeg image; EE execution retains the coding toolchain image. Community adds Python, pip and venv to its image and runs a separate execution service with a writable workspace volume. The build-time `codingSupported=false` restriction is removed. Trusted local CLI execution has no new deployment dependency.

## Security and opt-in

Commands execute in the trusted execution-worker container. They **can read another run's checkout on the shared execution volume**. Command allowlists, working-directory checks, filtered environments and run directories do not provide a chroot, filesystem sandbox or same-user isolation. The deployment split keeps ordinary support/CRM workloads out of that process and volume. Execution stays explicitly opt-in.

Only an authorized conversation owner can enable `execution_enabled`; it defaults to false. Model turn payloads cannot set it. Enabling requires a safe turn boundary and cancels the prior paused run; the next user message starts a separately projected execution-enabled successor. Disabling cancels the execution run and the next message starts an ordinary successor. Existing runs retain their original tools and routing. Permissions and publication approvals still apply. Repository attachment can target a workspace conversation without a task; Forge is optional.

Execution workers require inline `AGENT_RUNTIME_APP_CONFIG` with environment-based secret references. They reject `@file` configuration and do not mount app config. Linux dumpability is disabled before loading credentials, core limits are zero, and container capabilities are dropped. Agent child environments exclude runtime secrets. These measures reduce concrete credential exposure; they do not establish tenant isolation.

## Python and private files

`run_python` takes `source`, optional `output_paths`, and `timeout_seconds` (default 120, maximum 300). It invokes structured `python3` arguments through the command executor with cancellation and bounded output (64 KiB each for stdout/stderr). Every call starts a fresh interpreter. Files and packages persist within the same run. Additional packages use the existing command/approval path; a version pin is not a safety classification.

Analysis without a repository uses the existing workspace lease with a scratch directory on the execution volume. The private venv, pip cache, temporary files and HOME all live beneath `.agent-runtime/python` in that workspace. The 512 MiB scratch size limit is checked before and after execution and monitored during commands; over-limit commands are cancelled. This is an operational limit, not an adversarial filesystem quota in this trusted container.

Scratch cleanup is terminal-only. A versioned terminal workflow activity runs cleanup on the original worker queue/volume, including paused cancellation; it can retry across worker restarts. There is no inactivity deadline or reaper. A paused conversation can reuse its run's files; an ended run cannot. Repository leases retain their existing lifecycle. Files and package environments do not transfer to successors. Tool output reports workspace identity, availability and expiry, and successor context explicitly reports unavailable state. Publish important outputs before finishing a turn. Consider a persisted reaper only if volume usage warrants it.

`publish_outputs` or `run_python.output_paths` explicitly selects regular CSV, PNG, JSON and TXT files below the workspace. Escaping symlinks, traversal, devices, empty files and oversized outputs are rejected. Limits: ten files, 10 MiB each, 25 MiB total. The existing private binary-upload and host-storage path produces durable private artifact references, authenticated downloads and PNG previews. Artifacts outlive scratch cleanup; the app artifact provider must be configured even when browser execution is disabled.

## Interrupted external effects

Before launching commands, Python, push or PR tools, the native checkpoint records the call as started with an unknown outcome. Failure to persist prevents launch. Recovery returns an explicit unknown-outcome tool result to the model instead of deliberately repeating that call. Cancellation carries the same markers through terminal events and successor context. The assistant must disclose uncertainty and inspect remote state before retrying. A marker can conservatively report uncertainty even if loss happened just before launch. This is not an exactly-once guarantee.

## Optional TypeSafe review

Operators configure `TYPESAFE_API_KEY` and `AGENT_RUNTIME_TYPESAFE_ENABLED=true`. Requests use the fixed official `https://api.typesafe.ai/v1/systemone` endpoint, never the executing model's endpoint. Model defaults to `jev-latest` (`AGENT_RUNTIME_TYPESAFE_MODEL`); confidence threshold defaults to 0.95 (`AGENT_RUNTIME_TYPESAFE_THRESHOLD`). Enabling review sends selected command and repository context to an external provider.

Automatic approval requires the separate explicit setting `AGENT_RUNTIME_TYPESAFE_AUTO_APPROVE=true`. **Leave it disabled until representative live evaluations pass**:

```sh
AGENT_RUNTIME_TYPESAFE_LIVE_EVAL=true go test ./internal/runtime -run TestTypeSafeLiveEvaluations -count=1 -v
```

Provide the key through the operator secret manager. Offline routing tests are not evidence of the provider model's judgment quality. Extend `internal/runtime/testdata/typesafe_evaluations.json` for deployment-specific workflows.

Review applies only at execution-tool approval boundaries in `risk_based` mode. Human-only and explicit `never` modes remain unchanged. Hard permission denials cannot be overridden. Trusted human authorization is separated from repository content and model arguments; scripts/manifests and publication branch/diffs are included when necessary. Missing context, credential-bearing evidence, uncertain or destructive decisions require a human. Known environment, run-model and callback credentials are redacted before review; if that makes meaningful review impossible, no request is sent. No key, timeout, API error or malformed response preserves the existing risk-label policy. Provider failure is never interpreted as approval.

Decisions are bound to the pending call and context fingerprint and revalidated before execution. Existing interaction IDs and approval reconciliation are reused. Reviewer usage is journaled/emitted separately from parent-model usage and pricing. There is no new reviewer service or general execution ledger.
