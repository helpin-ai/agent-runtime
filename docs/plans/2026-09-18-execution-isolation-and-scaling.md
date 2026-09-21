# Execution isolation and scaling

Settled 2026-09-18. This plan finishes the opt-in execution work on `feat/python-analysis` and defines how the execution lane isolates tenants and scales after launch. It supersedes the deployment, concurrency and Community sections of [2026-09-17-opt-in-execution.md](../2026-09-17-opt-in-execution.md) where they differ, and it keeps the [sandbox-platform proposal](2026-09-17-unified-agent-sandbox-execution.md) superseded.

## 1. Decisions

| Area | Decision |
|---|---|
| Execution concurrency (EE) | 1 activity per execution pod. Capacity is the replica count. |
| Execution storage (EE) | ReadWriteMany via JuiceFS CSI so several execution pods share one workspace filesystem. Per-run lock for fencing. |
| Isolation (EE) | Landlock ruleset applied to every sandboxed command, confining it to its run directory. Fail closed below Landlock ABI 2. |
| Node placement (EE) | Execution pods pinned to nodes with kernel 6.5 (ax162, fsn1). |
| Community | One worker process serving all queues, coding and non-coding together. Landlock best effort. Contention accepted. |
| TypeSafe | Optional, off by default, never authorizes external effects. Unchanged. |
| Rejected | Per-tenant execution deployments. Bubblewrap. The sandbox manager, egress proxy and runner protocol remain deferred. |

Sequence: finish the branch, launch for vetted workspaces on the current single pod, ship Landlock, then JuiceFS and replicas. Landlock does not depend on the storage change and carries over unchanged.

Status 2026-09-18: the code for stages 1, 3 and 4 and the Community packaging is on `feat/python-analysis` (worker roles, Landlock shim, run lock, RWX manifests, single Community worker). Staging rollout of the JuiceFS claim, node labels and the release gates in section 4 remain to be run.

## 2. Why this shape

The only cross-tenant path on the execution worker is a command spawned by `run_command` or `run_python`. File, search, symbol and git tools resolve every path through the workspace guard and cannot leave the run directory; scanners run fixed binaries with runtime-controlled arguments; host credentials never enter a command environment. Confining the command's process tree is therefore sufficient, and Landlock does that without namespaces, privileges, seccomp changes or new pods.

Concurrency 1 per pod is a capacity model, not a security requirement. Landlock isolates files; the per-pod limit isolates CPU and memory and keeps a memory kill from taking several runs. Scaling by pod count needs a shared filesystem, which is what the RWX volume provides.

Community is single-tenant. The shared volume is not a confidentiality problem there, and the remaining concern, a compromised run reading the operator's own secrets, is already covered by inline app configuration and the non-dumpable worker.

## 3. Cluster facts (production, 2026-09-18)

Kubernetes 1.31.4, containerd 1.7.24, Hetzner. Mixed node pool:

| Nodes | OS / kernel | Landlock |
|---|---|---|
| ax162 × 2 (fsn1) | Ubuntu 22.04 / 6.5 | ABI 3 |
| ax52 × 3, master-4, master-5 | Ubuntu 22.04 / 5.15 | ABI 1 (cross-directory rename and link forbidden; do not use) |
| all others | Ubuntu 20.04 / 5.4 | none |

Verified with probe pods under `RuntimeDefault` seccomp, non-root, all capabilities dropped: `landlock_create_ruleset` returns ABI 1 on 5.15 and ABI 3 on 6.5, so the containerd default profile passes the Landlock syscalls and no Localhost seccomp profile is needed. `kernel.unprivileged_userns_clone` is 1 and `unshare -Ur` succeeds when seccomp is unconfined, so Bubblewrap would be possible but would require relaxing seccomp on the execution pod; it is not the chosen route. Landlock network rules need kernel 6.7 and are not available on any node.

## 4. Stage 1: finish the branch

Before the release gates:

- Set execution concurrency to 4 in `WorkerQueues`; Landlock and run locks isolate files, while pod CPU and memory remain shared and bounded.
- Choose the TypeSafe threshold defaults and cite the runtime's own live evaluation figure for them in the opt-in doc, not the ad hoc trial's figure.
- Commit the outstanding work in both repositories.
- Render the production overlay and confirm: execution worker receives inline `AGENT_RUNTIME_APP_CONFIG`; shared workers keep 2 replicas and rolling updates; Community compose is as described in section 8.
- Run the gates: the six live e2e scripts under Helpin's `frontend/e2e/agent-runtime-live`, `scripts/container-python-smoke.sh` on both images, `TestTypeSafeLiveEvaluations`, and one deliberate pod kill during a running command to observe the unknown-outcome path.

Cleanup that may follow in a separate commit: dock `SendMessage` holding the turn lock across the runtime start call; the `-execution` agent-ID suffix as the execution signal; dead `cmd.Env` assignments in the git provider; the duplicated redaction and widen-tools helpers; the double scratch cleanup on the durable path.

## 5. Stage 2: launch posture

Execution stays opt-in behind `settings.manage`, enabled only for workspaces that have been told their commands share a worker and a volume with other opted-in workspaces. This is a trust boundary, not an isolation boundary, until stage 3 ships.

Watch in the first week: execution queue wait time, execution pod memory against its limit, and the count of runs ending with `interrupted_external_effects` markers.

## 6. Stage 3: Landlock

### Mechanism

Go cannot run code in the child between fork and exec, so the worker re-executes itself: `runCommand` launches the worker binary with a `landlock-exec` subcommand, which applies the ruleset to itself and then `exec`s the real program with the prepared argv, environment, working directory and process group. This is the one seam; `run_python` already routes through it.

Ruleset:

| Path | Access |
|---|---|
| `/usr`, `/lib`, `/lib64`, `/bin`, `/sbin`, `/etc`, `/opt`, `/app` | read, execute |
| Operator-owned toolchain roots selected from the worker `PATH` (the active NVM version, snap Go, or rustup toolchains) | read, execute |
| `/proc`, `/dev` | read; write on `/dev/null`, `/dev/zero`, `/dev/urandom` |
| the run's directory (repository checkouts, analysis scratch, Python state) | read, write, execute |
| everything else, including sibling run directories | no access |

Before exec, point `HOME`, `TMPDIR`, `PIP_CACHE_DIR`, `npm_config_cache`, `GOPATH`, `GOCACHE`, `GOMODCACHE` and `CARGO_HOME` at directories under the run root. Keep rustup's installed toolchain read-only while Cargo's writable home remains run-local. Package installation and builds continue to work; caches become per run.

### Policy

- `--coding` probes the Landlock ABI at startup and exits below ABI 2 unless `AGENT_RUNTIME_EXECUTION_ISOLATION=none` is set explicitly. A pod scheduled onto a 5.4 or 5.15 node stops instead of running unconfined.
- The local CLI executor is exempt; it leases the user's real directory on purpose.
- A command denied by the ruleset returns a tool result stating that the path is outside the run's workspace, so the model corrects rather than retries.

### Placement

Label the ax162 nodes (for example `agent-runtime/landlock=abi3`) and set required node affinity on the execution deployment. Confirm the existing workspace volume is in fsn1 before changing affinity.

### Tests

- Integration: through the shim, `cat` on a sibling run directory fails with permission denied; `pip install` into the private venv succeeds; `go build` with the redirected cache succeeds.
- Startup: `--coding` on a host without Landlock exits with the documented message; with the opt-out it starts and logs once.

### What it does not cover

Network egress from commands is unrestricted until nodes reach kernel 6.7, when Landlock network rules can block outbound connections from `run_python` unless requested. Runs on the same pod can see each other's process list; arguments and environments carry nothing sensitive. Neither is a cross-tenant data leak.

Once this stage ships, execution may be enabled for any workspace administrator.

## 7. Stage 4: JuiceFS RWX and replicas

### Storage

JuiceFS CSI, community edition. Data chunks in an S3 bucket; metadata in Redis or TiKV. Postgres is supported but is the slowest engine for the small-file pattern of `npm install`, git object writes and Go's build cache, and placing it on the product database adds an execution-critical dependency to it.

Keep repository checkouts and analysis outputs on JuiceFS. Keep regenerable
toolchain state (Python venv, package download caches, Go caches, Cargo home and
target) on a size-limited local `emptyDir` under a hashed per-run directory; a
run that moves pods rebuilds it. Project-owned trees such as `node_modules`
remain in the checkout unless their package manager is configured separately.

The metadata engine is on the critical path for all execution and its loss loses every file, since the bucket holds chunks not files. Backups and a restore drill are part of the rollout. The CSI driver runs a privileged mount pod per node; that belongs to the infrastructure repository's review.

### Fencing

Temporal runs one activity per run at a time, but a partitioned pod whose heartbeat timed out can still be writing when the retry starts elsewhere. Before executing an activity, the worker takes a POSIX lock on a file in the run directory and holds it for the activity's duration. JuiceFS locks are backed by the metadata engine and are cluster-wide. A lock already held makes the retry fail with a clear message rather than corrupt the checkout.

### Deployment

Execution becomes a Deployment with N replicas, concurrency 1 each, rolling updates, node affinity from stage 3, the JuiceFS volume mounted RWX. Overlays set 2 replicas in staging and 5 in production; production needs the `juicefs-r2` StorageClass created before its overlay is applied, or the claim never binds. Production runs `AGENT_RUNTIME_EXECUTION_ISOLATION=landlock` with the node affinity; staging runs `best_effort` with no affinity, so its pods confine only when they land on a Landlock-capable node. Admission is unchanged: it asks whether the execution queue has live pollers. A queue-depth autoscaler can follow later.

Verify Landlock on the JuiceFS mount with the stage 3 integration tests before cutover.

## 8. Community

One worker process polls all four queues, so command-capable runs and ordinary chat share a process and a volume. Add an all-queues startup mode to the worker (the shared-worker rejection of command-capable runs is a runtime flag; the all-queues mode sets it). Community compose then needs one worker service with the writable workspace volume, not a separate execution service.

Landlock is applied when the host kernel and Docker seccomp profile allow it and skipped with a single log line when they do not. No fail-closed behaviour and no host requirements. Keep the inline app configuration requirement and the dumpability hardening.

## 9. TypeSafe

Unchanged: optional, off by default, model pinned, external sends, installs and publication never auto-approved, external `send_requested` diagnostic only. After Landlock its role is prompt reduction on local work and a signal for outbound network use in `run_python`, which it only ever routes to a human. Auto-approval remains gated on the live evaluation and on real approval history.

## 10. Acceptance

- Execution lane serves up to four concurrent activities per pod; a fifth queues rather than co-executes.
- A command in one run cannot read another run's directory on the same pod; the same holds across pods on the RWX volume.
- `pip install`, `npm install`, `go build` and `cargo build` succeed under the ruleset with redirected caches.
- An execution pod scheduled onto a non-Landlock node refuses to serve the queue.
- A pod killed mid-command yields an unknown-outcome tool result on the retry, on a different pod, with the checkout intact and the lock released.
- Rolling update of the execution deployment completes with no run failures.
- Community single-worker install runs ordinary chat, coding and `run_python` from one service, with and without Landlock available.
- Existing coding, browser-artifact and product-approval tests remain green.

## 11. Deferred

Network egress control per invocation (kernel 6.7 or the egress proxy). Per-run isolation stronger than Landlock (gVisor or the sandbox manager). Shared read-only build caches across runs. Scale-to-zero execution. Each is revisited when a customer requirement needs it, not before.
