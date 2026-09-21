# Unified agent sandbox execution and automatic access review

> **Superseded proposal.** Implement the [bounded opt-in execution design](../2026-09-17-opt-in-execution.md). The sandbox managers, pod provisioning, egress proxy, recovery bundles and idle reaper below are deferred proposals, not existing code being removed. The separate shared/execution deployments remain; image unification is optional.

## 1. Goal and agreed behavior

Use one Agent Runtime worker implementation for all agents. Run agent-directed programs and repository operations in reusable sandbox containers or pods, selected from administrator-defined execution profiles.

**Ask Agent can analyze data, write code, run tests, create branches, push changes, and open PRs directly when its effective tools and permissions authorize those operations.** Forge and other saved agents remain useful configurations and optional delegation targets; they are not mandatory execution boundaries.

The infrastructure distinction is between trusted orchestration and sandbox execution, not between normal agents and coding agents. A Python script and a repository build use the same sandbox lifecycle, permission review, artifact transport, and recovery mechanisms.

Agreed behavior:

- Keep the model loop, approvals, billing, and host-app callbacks in ordinary Temporal workers.
- Execute commands and filesystem/repository tools through a common sandbox executor.
- Support Kubernetes pods and Docker containers through deployment-selected backends. Backend choice is independent of Community versus SaaS edition.
- Reuse files, installed packages, and repository working state across follow-ups. Start a fresh process per ordinary execution call; do not implement persistent Python variables or notebook kernels.
- Delete idle sandboxes after one hour, or when their agent run completes, fails, or is cancelled, subject to the unpublished-work preservation rule below.
- Finishing an assistant turn does not terminate the run or delete its sandbox.
- Let the agent reuse an existing sandbox or request a new one for unrelated work or a different toolchain.
- Review requested external access and package installation before execution. Support API reads and writes.
- Accept credentials supplied in chat without adding a connection manager or vault. Do not provide ambient worker credentials to scripts.
- Use explicit capabilities and authorization for repository mutations and external writes. Neither the agent's display name nor its chosen image grants permission.

This plan supersedes the Python-only plan. It removes nested Bubblewrap sandboxing inside workers, worker-affine scratch environments, mandatory coding delegation, and the permanent separate coding-worker architecture.

## 2. Architecture and ownership

```text
Helpin: agent configuration, actor permissions, approvals UI, private artifacts
    |
Agent Runtime worker: model loop, contextual access review, durable execution records
    |
Sandbox manager: fixed templates, leases, resource limits, lifecycle, operation dispatch
    |                                  |
Docker backend                    Kubernetes backend
reusable container               reusable pod
    |                                  |
approved execution image + runner + private workspace
    |
managed egress proxy: invocation-scoped destination access
```

### Shared sandbox manager

Implement a runtime-owned manager with provider operations to create, inspect, execute, cancel, transfer selected files, and destroy sandboxes. Keep provider-specific behavior behind the Docker and Kubernetes implementations.

Run the manager as a trusted internal component deployed separately from untrusted sandboxes. The same manager API and application logic serve both backends; only the provider and deployment wiring differ. This is not another agent worker or model loop.

The manager accepts authenticated, scoped requests identifying stored execution records. It does not expose arbitrary Docker/Kubernetes specifications, host paths, runtime sockets, or privileged execution flags.

Persist sandbox ownership and provider identity in shared runtime storage. Any ordinary worker can address the same sandbox through the manager. Worker restarts do not inherently destroy sandbox state, and later turns do not depend on returning to the original worker.

Use transactional ownership, revision checks, and fencing to serialize operations within a sandbox. Provider creation must be idempotent using trusted app/run/sandbox identity and provider resource labels. A retry must discover the existing resource rather than create a duplicate.

### Execution profiles and images

Provide two initial profiles:

| Profile | Image contents | Typical workspace |
|---|---|---|
| `python` | Python, pip, pandas, NumPy, matplotlib, requests | Analysis files and private Python environment |
| `development` | The existing supported coding toolchain: Git, Python, Node/package managers, Go, Rust, Make/build tools | Scratch files, repository checkouts, dependencies, build outputs |

Both images contain the same trusted runner protocol. Pin images by digest and record that digest in sandbox and execution records.

The model may request a profile name from the app's allowed list. The runtime maps that name to a fixed template and image; it cannot supply an arbitrary image or change mounts, service accounts, privileges, or resource ceilings.

Default Python-only work to `python`; repository execution defaults to `development`. Use existing compatible sandboxes when possible. Do not replace an existing sandbox's image in place: create another sandbox and explicitly transfer selected authorized files when a different profile is needed.

Image contents are not authorization. The development image may be available while push, PR creation, or external network access remains disallowed.

## 3. Model-facing tools and capability policy

### Preserve existing execution tools

Keep the established names and payloads for repository discovery/checkout, file reads/writes/edits/patches, search and symbol tools, commands, Git operations, tests, scans, and previews wherever possible.

Route all execution-local operations through a sandbox execution context. Remove assumptions that a workspace root is a path accessible inside the Temporal worker. Repository roots are logical paths within a sandbox, with app/run ownership checked again at dispatch.

Extend relevant tool inputs with an optional `sandbox_id` and additive access-request fields where needed. Omitted sandbox selection resolves from the run's current sandbox and repository alias; ambiguous selection returns a repair-oriented error.

Keep host product operations such as creating a PR through an authenticated provider API in the host/provider layer. They refer to the verified sandbox repository/branch result and continue to enforce actor and repository permissions.

### Python convenience tool

Add `run_python` as a convenience wrapper over the same executor and access-review path, not a separate sandbox system.

| Input | Behavior |
|---|---|
| `code` | Python source, maximum 64 KiB |
| `purpose` | Short explanation of the intended analysis or action |
| `sandbox_id` | Optional sandbox belonging to this run |
| `new_sandbox` | Request a new Python sandbox; mutually exclusive with `sandbox_id` |
| `network_hosts` | Exact requested HTTPS hostnames; no wildcards; empty means no external access |
| `packages` | Additional Python packages as `name==version` |
| `input_files` | Named UTF-8 files or authorized private artifact references |
| `output_files` | Relative paths to publish as private artifacts |
| `timeout_seconds` | Default 120 seconds, maximum 300 seconds |

Return sandbox ID, state revision, expiry time, structured status, bounded stdout/stderr, exit information, truncation flags, and private artifact references.

### Generic sandbox management

Add `manage_sandbox` with `list`, `create`, and `close` actions. Creation accepts an allowed profile name; closure accepts a sandbox ID owned by the current run. Listing returns compatible profiles, repository associations, package summaries, storage usage, and expiry times.

Omitted sandbox selection reuses the run's current compatible sandbox or creates its first sandbox lazily. Creation makes a sandbox current without deleting older sandboxes.

Default to two retained sandboxes per run. Reject further creation at the limit with an actionable response; the agent can close one it no longer needs. Closure follows the unpublished-work preservation rule.

Derive app, workspace, actor, run, and tool-call identity from trusted execution context. Never accept them as unrestricted model-controlled authority.

### Ask Agent and other agents

Remove Ask Agent's prompt prohibition on coding and its enforced read-only repository restriction. Add the managed coding/file/command/Git tools to Ask Agent's configurable surface where the app and actor authorize them, and reconcile managed capabilities for pinned preset versions.

Directly completing an authorized coding request must not require creating an artificial task, switching the conversation target, or launching Forge. A workspace-targeted Ask Agent run can attach an authorized repository sandbox and deliver its branch/PR result to the current conversation.

Preserve saved-agent and per-run tool intersections, app capabilities, command restrictions, actor permissions, repository scope, and publication approvals. Repository resolution and credential callbacks must support an explicit authorized repository on a workspace run; do not bypass target-owned checks by inventing a task.

General command execution inherently permits modifying writable sandbox files, including through interpreters. Do not present removal of a named file-write tool as a read-only guarantee while writable command execution remains available. Enforce genuinely read-only repository access through the mounted filesystem policy. Similarly, destination-only HTTPS filtering cannot prove application-level read-only access; scope issued credentials and use contextual review without claiming deterministic inspection of encrypted operations.

Update prompts to choose tools based on the work, reuse sandbox state for follow-ups, and delegate only when specialization, parallelism, or isolation is useful. Apply coding instructions when repository work is actually active, rather than to every run merely possessing command permission.

## 4. Generic automatic access review

### Decision model

Add a reusable review interface with `allow`, `ask_user`, and `deny` outcomes. Apply it to sandbox external access, package installation, and execution requests that need permission escalation, regardless of profile or agent name.

Preserve existing host-product approval rules in this release. A sandbox grant does not satisfy an unrelated approval for publishing, merging, deleting, or changing product records.

Local operations within existing tool and sandbox permissions do not require an additional model review on every call. A script/command requesting external access or installing executable dependencies does.

The reviewer receives the exact script or command, relevant trusted user request and authorization history, sandbox revision, staged-input identity, repository context when applicable, dependency manifest, requested destinations, expected external effects, and versioned policy.

Treat code comments, tool results, downloaded material, repository instructions, and the executing model's justification as untrusted evidence. They do not independently authorize a mutation or data transfer.

Use a separate model invocation without tools, using the parent run's configured model/provider and credential path. Use a 30-second timeout and validated structured output. Bill review usage through the existing parent-run usage/checkpoint path, with purpose `access_review`.

- `allow`: authorization is sufficient under policy.
- `ask_user`: authorization is insufficient or the reviewer cannot establish it.
- `deny`: a hard policy restriction applies.

Reviewer timeout, parse failure, or other review failure grants nothing and uses the existing human approval flow. Model-credential and budget failures retain existing run-level handling. User approval cannot override hard tenant, repository, destination, or platform restrictions.

### Grants and human approval

Bind each decision to app, workspace, run, tool-call ID, sandbox ID/revision/image, policy version, and a fingerprint of the exact execution inputs, dependencies, destinations, and limits.

Each grant authorizes one matching invocation. Sandbox reuse does not preserve old network grants. Changes to the code, command, inputs, image, dependencies, destinations, or sandbox revision require a new decision.

Persist status, concise rationale, risk assessment, decision source, and execution state. Human approvals use existing interaction IDs/cards and resume the stored matching operation directly, not a reconstructed model call.

While human approval is pending, do not launch the program or reserve an execution slot. A sandbox can still idle-expire; approval then returns state unavailable rather than executing against an empty replacement.

### Networking and credentials

Use a managed forward proxy for approved HTTPS destinations on port 443. Block direct external access. Resolve and validate destinations at the proxy, connect to validated addresses, and reject private, loopback, link-local, metadata, and other non-public targets. Ignore ambient proxy settings from workers.

Undeclared redirect destinations fail. HTTPS Git remotes are supported; SSH remotes requiring port 22 are outside v1 and must use an available HTTPS remote or return an actionable unsupported-transport error.

The proxy enforces destination and invocation identity, not application-level intent. It does not decrypt HTTPS or claim to inspect request bodies. The reviewer evaluates uploads, writes, and credential use from the proposed operation and trusted authorization. It cannot guarantee that arbitrary downloaded package code is benign.

Use chat-supplied API credentials without adding a vault or connection UI. They remain subject to existing private conversation retention. Never duplicate headers, bodies, credentials, or raw scripts into operational logs or approval summaries. Credentials deliberately written into sandbox files persist until removed or the sandbox is deleted.

Keep host-managed Git/provider credentials outside arbitrary commands. Use short-lived, repository-scoped grants in the trusted Git/provider path. Fetch/checkout receives read scope; push/PR operations receive only the scope authorized for that exact operation. A generic network grant must not bypass repository write permission or expose an ambient push credential to a script.

### Package installation

All installation occurs inside sandboxes. Package installation cannot mutate the worker image, manager environment, or another run's dependencies.

For `run_python.packages`, resolve exact wheel versions and dependency hashes through a bounded preparation environment without user secrets or input files. Review that manifest with the execution request. Install verified wheels offline into a candidate environment, activating it only after successful installation. Reuse that environment in later calls.

Python convenience installation supports PyPI wheels; source builds, VCS dependencies, and alternate indexes are not part of that convenience API.

The development profile must retain existing project-build capability: commands such as npm install, pip install, cargo, and project build scripts may install dependencies and execute lifecycle/build code after contextual review. Do not silently disable source builds needed by existing coding workflows. Show manifest/lockfile context where available, preserve lockfile integrity checks, and enforce the same destination and resource limits. Unknown dependency behavior must not be represented as fully statically verified.

## 5. Backends, isolation, and reusable state

### Common runner and lifecycle

Each container/pod runs a small runner and retains private files, package environments, repository working changes, and a monotonically increasing state revision. Ordinary invocations start fresh processes and clean up their process trees on exit, timeout, or cancellation.

Preserve existing supported preview/process-session behavior as explicit managed sessions. They have separate lifecycle records and bounded lifetimes; do not allow accidental background processes to survive an ordinary command.

Serialize operations within each sandbox and revalidate approved state immediately before execution. A failed execution can leave filesystem changes; record a new revision and do not claim rollback.

Persist sandbox identity and status outside workers. Manager restarts reconcile provider resources by trusted labels and IDs. A worker restart must not delete a live sandbox. Container/pod loss returns `sandbox_unavailable`; do not silently recreate lost state under the old identity.

No notebook kernels, transparent sandbox migration, or guaranteed survival across sandbox/node loss in v1. Durable artifacts and repository recovery bundles provide explicit restoration inputs.

### Kubernetes backend

The manager creates reusable sandbox pods in a dedicated namespace using a fixed template and Kubernetes API client. Its service account has namespace-scoped sandbox lifecycle permissions; model inputs cannot choose service accounts, volumes, host namespaces, or pod specifications.

Sandbox pods run non-root, with a read-only root filesystem, dropped capabilities, no privilege escalation, seccomp, and no service-account token or worker secrets. Keep the writable workspace in a private size-limited volume retained for that pod's lifetime.

Use a configured gVisor RuntimeClass for the production untrusted-execution profile. Require the node runtime to be installed and tested; do not silently fall back if the configured RuntimeClass is unavailable. Document any explicitly selected standard-container deployment as a weaker isolation profile.

Install default-deny networking before sandbox admission. Allow inbound runner control only from the manager and outbound application traffic only through the egress proxy. Use a CNI that actually enforces NetworkPolicy, and test node/metadata/internal-service bypasses on the selected platform. The proxy resolves external names so direct sandbox DNS egress is not required.

Kubernetes owns container resource enforcement. Configure and verify PID limits at the supported node/runtime boundary; do not assume an ordinary pod resource field provides a per-pod PID quota.

### Docker Compose backend

Add the manager and egress proxy to Community Compose. Compose starts those services; the manager creates reusable sandbox containers dynamically through Docker Engine.

Only the trusted manager receives Docker Engine access. Its API accepts fixed sandbox operations, never arbitrary Docker payloads. Sandbox containers and agent workers receive no Docker socket. Document that manager compromise carries Docker-daemon authority; an operation-limited HTTP API is not a restriction on the daemon credential itself.

Use the same runner/images with non-root users, read-only root filesystems, dropped capabilities, no-new-privileges, seccomp, resource limits, and private writable storage. gVisor can be explicitly configured where supported; standard Docker must not be described as equivalent isolation.

Do not join sandboxes to Helpin's default application network. Enforce per-sandbox network isolation and proxy-only egress, including denial of host-gateway/internal-service access and sibling-sandbox access. An `internal` Docker network alone is not sufficient evidence of all these properties. Provider admission must verify the supported host firewall/network configuration and fail closed if enforcement is unavailable.

No sandbox gets host bind mounts, deployment credentials, Docker/Kubernetes API access, or another run's volumes. Unsupported host platforms report unavailable capability rather than executing inside the worker.

### Limits and retention

Initial defaults:

| Setting | Python profile | Development profile |
|---|---|---|
| Memory ceiling | 1 GiB | 4 GiB |
| CPU ceiling | 1 CPU | 2 CPUs |
| Process/thread ceiling | 64 | 256 |
| Writable workspace ceiling | 512 MiB | 10 GiB |
| Ordinary execution timeout | 120 seconds, maximum 300 | 120 seconds, maximum 900 |
| Idle expiry | 1 hour | 1 hour |

Use backend-supported storage enforcement and verify its behavior; do not claim that eviction-based ephemeral-storage accounting is an immediate hard filesystem quota. Treat hard storage enforcement as a deployment capability gate.

Default installation capacity: four active executions and sixteen retained sandboxes, additionally limited by app quotas and backend capacity. These limits belong to the shared manager, not to each individual worker. Default per-run retained sandbox limit is two. Administrators may configure capacities without changing model-facing permissions.

Reset idle expiry after execution or package preparation completes. Listing state does not extend it. Do not expire an active operation; explicitly bounded preview sessions also count as activity until their deadline.

Run completion, failure, cancellation, explicit closure, and idle expiry trigger cleanup. Cleanup terminates processes, revokes grants, deletes provider resources and scratch volumes, and retains published artifacts. Use a periodic reaper with persisted deadlines, rather than relying only on process-local timers.

### Preserve unpublished repository work

Before intentionally deleting a dirty repository sandbox, persist a private recovery bundle containing unpushed commits, tracked binary/text changes, and non-ignored untracked files. Include ignored files only when explicitly selected; do not archive dependency caches or credentials by default.

Use the existing host-private storage boundary and a separate recovery-bundle limit of 1 GiB. Record its repository revision and restoration instructions. Publishing a recovery bundle does not push a branch or expose it publicly.

If recovery exceeds limits or upload fails, stop processes and network access, mark cleanup blocked, and retain the workspace storage under existing quotas. Surface the condition and retry cleanup; do not silently discard unpublished code. New admissions may be rejected when retained storage exhausts capacity.

Scratch analysis state may expire without archival unless selected as an output. Explain this distinction in tool descriptions. Unexpected pod/container/node loss may still lose unpublished state; this feature is not a backup guarantee.

## 6. Durable execution, artifacts, and user experience

### Execution records and retries

Persist an execution record before launching any command or mutating repository operation. Key it by trusted app/run/tool-call identity and bind it to the sandbox revision and permission decision.

Record preparation, review, approval, started, completed, failed, cancelled, and uncertain/interrupted outcomes. The manager and runner must deduplicate execution IDs across worker/manager retries and retain completed results until they are durably acknowledged.

Never automatically replay a started script, command, or external mutation. If a worker loses contact, reconnect and retrieve status/result. If the outcome cannot be established, report it as uncertain. A POST or push may already have succeeded.

Temporal retries, native checkpoint recovery, and approval reconciliation must consult this record. Duplicate approval signals must not execute a second copy. Reads can be retried only under their established read-only semantics.

On an undeclared-host failure, do not automatically restart the whole script with broader permission. Reassess earlier local and external effects before proposing another invocation.

### Files and artifacts

Generalize artifact delivery to `sandbox_output` and private repository recovery bundles while preserving existing browser and coding artifact formats.

Publish only explicitly selected regular files within the authorized sandbox workspace. Reject traversal, symlinks escaping the workspace, devices, and oversized output. Default ordinary-output limits: ten files, 10 MiB each, 25 MiB total, and 64 KiB each for stdout/stderr.

Transfer files through the manager and authenticated app-specific host callback. Never return storage keys or permanent public URLs. Reusing an artifact requires workspace authorization and trusted retrieval.

Show CSV, JSON, text, PNG, PDF, and generic downloads. Preview supported images and serve other formats as attachments. Keep artifact retention independent of sandbox expiry.

### Helpin UI and policy projection

Project sandbox identity, profile, availability, expiry, and repository association through tool results and capability context. Make state-loss and capacity failures actionable.

Reuse existing human approval cards. Add compact auto-review statuses and show intended actions, destinations, packages, and consequences without credentials. Record reviewer usage as part of the run.

Show branch/PR links and validation evidence from direct Ask Agent coding in the current conversation. Do not require a task page or a specialist launch to render these results.

Keep operational logs limited to identities, timing, status, limits, destination hostnames, and sanitized rationale. Exclude raw source, request payloads, headers, credential values, and file contents.

## 7. Cross-repository implementation and migration

### Agent Runtime

- Add shared manager/provider/runner contracts, Docker and Kubernetes backends, leases, fencing, execution records, and cleanup.
- Add automatic access review, invocation-scoped proxy grants, usage checkpointing, and human-approval integration.
- Move commands, filesystem tools, search/symbol operations, repository checkout/Git operations, scans, and previews onto the executor. Preserve current capability behavior or document an explicit unavailable capability; never fall back to worker-local execution.
- Add profile/image configuration and expose profile capabilities through app-scoped registration and effective-tool policy.
- Replace coding-poller admission and the coding-worker flag with sandbox capability admission. Missing execution capacity must not prevent an otherwise read-only chat from starting; fail the unavailable execution tool clearly when needed.
- Preserve explicitly trusted local CLI execution as a separate local backend, with no new requirement for Docker or Kubernetes. Server execution must never silently select it.

### Helpin

- Update tool catalog schemas, Ask Agent managed tools/prompts, custom-agent validation, capability projection, and pinned-version reconciliation.
- Support explicit repository attachment on workspace-targeted runs with existing actor/repository authorization.
- Extend private artifact upload/retrieval and UI rendering without breaking existing browser or coding artifacts.
- Preserve billing/usage preflight and ensure reviewer usage is included.
- Remove mandatory coding delegation and stale read-only Dock enforcement wherever it exists, not only in the system prompt.

### SDK and wire compatibility

Where shared SDK types are needed, add optional sandbox/execution references rather than repurposing a worker-local root path. Version the runner protocol and sandbox manager API, keeping image/manager compatibility explicit.

Use additive storage migrations and optional wire fields. Keep existing public tool names and aliases unless a specific incompatibility requires an explicit transition.

### Delivery sequence

1. Implement the common contracts, manager, provider backends, runner, isolation checks, review path, artifacts, and leases behind a feature flag.
2. Deliver the Python convenience tool as the first end-to-end consumer on both Docker and Kubernetes. This validates the common infrastructure, not a separate Python architecture.
3. Migrate all existing remote coding/repository execution capabilities onto the same executor, preserving branch/PR, validation, scan, preview, and continuation behavior.
4. Enable direct Ask Agent coding according to its effective permissions and validate workspace-targeted repository flows.
5. Inventory old coding runs and drain or explicitly resolve queued, paused, and active runs while legacy workers remain available. Do not rewrite an old Temporal workflow's queue or execution provenance in place.
6. Stop routing new work to the coding queue, verify no legacy run depends on it, then remove the dedicated coding-worker deployment, routing, and image packaging. Retain historical metadata decoding for old run records.

Final-state deployment has ordinary agent workers, a shared sandbox manager/proxy, and execution images. It has no separate coding agent engine or mandatory coding worker. Existing interactive/autonomous queue separation can remain for scheduling and latency.

Ship compatible Helpin/SDK support before enabling runtime capabilities. Provide matching Community Compose and Kubernetes manifests, fixed templates, secrets/RBAC wiring, resource quotas, cleanup configuration, and operator documentation. Enabling only one backend does not require installing the other.

## 8. Verification and acceptance criteria

### Architecture and direct execution

- The same worker handles analysis and repository work without a coding flag or mandatory delegation.
- Ask Agent can directly check out an authorized repository, edit code, run tests, create a branch, push, and open a PR when its tools and actor permissions allow it.
- Selecting a development image cannot grant host/provider permissions or runtime credentials. Raw commands cannot bypass credential-issuance checks, and read-only repository mounts remain read-only even when interpreters are available.
- Normal read-only conversations work without a configured sandbox backend.
- Docker and Kubernetes pass the same executor contract suite using the same runner image.
- Trusted local CLI operation remains functional without remote sandbox infrastructure.

### State and lifecycle

- Follow-ups reuse downloaded files, dependencies, and repository changes without repeating setup.
- Fresh processes do not preserve Python globals; files do persist.
- The model can create a fresh sandbox and later select the previous one.
- Worker and manager retries do not duplicate containers or commands; worker replacement can reconnect to the same sandbox.
- Idle expiry, terminal cleanup, explicit closure, retained-count limits, and concurrency are enforced without deleting active work.
- Provider loss is reported honestly; no silent replacement or fake continuity occurs.
- Dirty-work recovery bundles preserve unpushed changes before intentional deletion; failed archival blocks destructive cleanup.

### Authorization and side effects

- Authorized reads/writes and package installs can pass auto-review; insufficient authorization requests human approval.
- Hard-denied destinations, tenants, repositories, and operations remain blocked after any reviewer/user decision.
- Changed execution inputs or sandbox revision invalidate approval.
- Reusing a sandbox does not retain network or Git publication grants.
- Temporal retries, checkpoint recovery, duplicate approvals, and worker crashes do not replay started scripts, pushes, or API writes.
- Unknown outcomes remain uncertain until checked; no exactly-once claim is made for external APIs.
- Review cost is recorded once and survives checkpoint recovery.

### Isolation and provider enforcement

- No sandbox can access worker secrets, runtime management credentials, Docker sockets, Kubernetes tokens, host mounts, sibling files, or manager control channels.
- Direct networking, proxy bypass, DNS rebinding, redirects, IPv6, metadata endpoints, host gateways, and internal-service access are tested on both deployment profiles.
- Resource limits and process-tree cancellation are enforced on the real backends; unsupported runtime/CNI/storage/PID controls fail admission.
- Kubernetes production tests use the configured gVisor RuntimeClass. Standard Docker results are reported with their actual isolation limitations.

### Capability parity and product integration

- Preserve existing repository checkout, multi-repository selection, file/edit/patch behavior, search/symbol tools, tests, scanners, Git finalization, PR creation, and managed previews.
- Browser artifacts and existing product-tool approvals remain compatible.
- Private downloads, charts, input-artifact reuse, recovery bundles, and cleanup authorization are workspace-scoped.
- Pinned Ask Agent versions receive the intended capability update; deliberately restricted agents remain restricted.
- Workspace-targeted direct coding does not require creating an artificial task or changing the conversation's target.

### End-to-end release gates

On both Community Docker Compose and Kubernetes, demonstrate:

1. Ask Agent fetches API data, installs a reviewed package, generates a CSV/chart, and answers a follow-up using retained state.
2. The same Ask Agent directly changes an authorized repository, runs validation, pushes a branch, and opens a PR without launching Forge.
3. A new sandbox/profile is selected when needed without transferring permissions implicitly.
4. Chat-supplied API credentials work for an authorized write; an unauthorized transfer is paused or denied.
5. Human approval, denied destinations, sandbox expiry, worker replacement, uncertain mutation recovery, and dirty-work archival behave as specified.

Run focused tests in all affected repositories, backend build/vet checks, relevant frontend tests/type checks, SDK compatibility tests, provider contract tests, and real isolation/integration checks before enabling the feature. Use existing coding smoke scenarios as mandatory migration tests before retiring legacy workers.

## 9. Reference documentation

- [Kubernetes Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
- [Kubernetes NetworkPolicy behavior and prerequisites](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
- [Kubernetes RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)
- [gVisor Kubernetes integration](https://gvisor.dev/docs/user_guide/quick_start/kubernetes/)
- [Docker Engine security and daemon authority](https://docs.docker.com/engine/security/)
- [Docker Compose networking](https://docs.docker.com/reference/compose-file/networks/)
- [Codex automatic approval review and sandbox separation](https://learn.chatgpt.com/docs/agent-approvals-security#automatic-approval-reviews)
