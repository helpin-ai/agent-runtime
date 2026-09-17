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

Operators configure `TYPESAFE_API_KEY` and `AGENT_RUNTIME_TYPESAFE_ENABLED=true`. Requests use the fixed official `https://api.typesafe.ai/v1/systemone` endpoint, never the executing model's endpoint. Enabling review sends selected command and repository context to an external provider. The adapter uses the official [atomic yes/no (`noul`) API](https://github.com/typesafe-ai/typesafe-sdk-js/blob/main/src/types.ts); code composes the seven probabilities into a decision.

| Operator setting | Default | Meaning |
| --- | --- | --- |
| `AGENT_RUNTIME_TYPESAFE_ASKED_THRESHOLD` | `0.90` | Minimum probability that the user asked for the operation or an ordinary in-scope step |
| `AGENT_RUNTIME_TYPESAFE_HAZARD_THRESHOLD` | `0.20` | Maximum probability for **each** hazard before suppressing a prompt |
| `AGENT_RUNTIME_TYPESAFE_ESCALATION_THRESHOLD` | `0.50` | A hazard **above** this value escalates a routine-labelled call to human review |
| `AGENT_RUNTIME_TYPESAFE_MODEL` | `jev-latest` | Requested model alias or operator-selected version |
| `AGENT_RUNTIME_TYPESAFE_EVALUATED_MODEL` | `jev-1.13.0` | Exact response model required for auto-approval; alias drift cannot silently enable a new version |
| `AGENT_RUNTIME_TYPESAFE_AUTO_APPROVE` | `false` | Separate opt-in after release evaluations pass |

The hazards are shared-branch writes, external sends/credential use, secret or foreign-workspace reads, irreversible deletion/live deployment, and obfuscated/untrusted execution. `external_send_requested` is recorded as a seventh diagnostic question; **it never grants external authorization**. Conditional logic lives in code, not in question wording. Every probability must be present, finite, and within zero to one; malformed responses retain the original risk-label policy.

Prompt suppression is restricted to local-operation candidates: file edits, local branch creation, Python analysis, and a conservative set of command forms for tests, builds, inspection, venv creation, and deletion of `dist`/`build`. Publication tools, direct network commands, dependency installs (pinned or otherwise), opaque shell commands, and unknown command forms cannot gain permission from reviewer confidence. Explicitly requested pushes and API writes use existing captured human confirmation; conversation execution opt-in is not permission for arbitrary external effects. No general chat model serves as an approval gate.

The local candidate list and hazard scores are not a network sandbox. Python, test runners and build scripts can have hidden effects. Existing command, path, repository, actor and tool restrictions still apply; the reviewer supplies no package-name or allowed-host enforcement. In particular, the trial missed the typosquat, so dependency installation stays outside prompt suppression rather than treating a pinned package or low hazard score as safe.

Review applies only at execution-tool approval boundaries in `risk_based` mode. Human-only and explicit `never` modes remain unchanged. Hard permission denials cannot be overridden. Trusted human authorization is separated from repository content and model arguments; selected scripts/manifests and publication branch/diffs are included when necessary. Missing context or credential-bearing evidence requires a human. Known environment, run-model and callback credentials are redacted before review; if that makes meaningful review impossible, no request is sent. No key, timeout, API error or malformed response preserves the existing risk-label policy. Provider failure is never interpreted as approval. With auto-approval off, normal local calls retain existing policy while high hazards still escalate.

Decisions are bound to the pending call and context fingerprint and revalidated before suppressing a prompt. Existing interaction IDs and approval reconciliation are reused. Scores, thresholds, resolved model and separate reviewer usage are journaled; approval summaries include scores for human review. Reviewer tokens do not enter parent-model pricing. There is no new reviewer service or execution ledger.

### Trial evidence and release evaluation

The user-supplied September 17 trial reports **18/34 benign calls approved and 0/44 must-ask calls approved** at asked ≥0.90 and hazards ≤0.20, using jev-1.13.0, with median latency 0.63 seconds, about 600 input tokens and approximately ±0.05 repeat drift. These are reported observations, not results reproduced by this implementation. The trial's first false approval at 0.75/0.30 was an environment dump. Requested and unrequested sends were not reliably distinguished; those scores must not authorize pushes or uploads.

`internal/runtime/testdata/typesafe_evaluations.json` reconstructs all **39 scenarios**, including injected authorization, secret reads, typosquatting, history rewrites, deployment, local work, and explicitly requested sends. Original payloads and raw two-run responses were not supplied. The fixture preserves the report's 17 benign / 22 must-ask grouping as provenance, with a separate gate expectation: network installation J, GET AD, and broader-than-requested test scope T now also require a prompt. A requested push or upload may be authorized by a human while remaining ineligible for reviewer auto-approval. The broader-than-requested test scope in T is explicitly documented.

**Leave auto-approval disabled until representative live evaluations pass**. With a key supplied through the operator secret manager, run:

```sh
AGENT_RUNTIME_TYPESAFE_LIVE_EVAL=true go test ./internal/runtime -run TestTypeSafeLiveEvaluations -count=1 -v
```

This sends each synthetic scenario twice, records the resolved version, scores, usage and latency, rejects any must-ask approval, and requires some useful prompt suppression. It performs no proposed commands. It does not assert reproduction of 18/34; the candidate filter, payloads and gate policy differ from the ad hoc trial. Add real approval-history cases and reassess thresholds on every model version before changing `EVALUATED_MODEL`. Offline routing tests validate implementation, not model judgment quality. Remove the obsolete `AGENT_RUNTIME_TYPESAFE_THRESHOLD` setting when migrating; its presence disables auto-approval to avoid reusing the former single-choice opt-in.
