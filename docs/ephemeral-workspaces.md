# Ephemeral coding workspaces

Coding checkouts, private dependency trees (`node_modules`, venvs and Rust
targets), build outputs and temporary edits use disk-backed `emptyDir`.
JuiceFS is not mounted in this coding path. Repository download/build caches
remain on the dedicated node-local hostPath, scoped to trusted
app/workspace/repository identity and reused across runs on that node.
Another node warms its own cache. Commands remain online; Landlock is unchanged.

## Ownership and recovery

- API starts coding workflows with a persisted ephemeral-workspace flag.
  Old workflow histories retain their original behavior.
- Temporal sessions keep preparation, execution, approval/input/auth resumes
  and terminal cleanup on the same worker. Four command activities per worker
  remain the limit. Up to 256 sessions can retain paused-run affinity without
  reserving a command execution slot; disk capacity remains a separate limit.
- Session heartbeat timeout is 30 seconds. Worker loss allows a new session
  on another worker, with up to three session allocations per workflow.
  Each session has a separate directory, including when assigned to a pod
  that still has files from an older attempt. Sessions expire after seven days;
  a later resume recreates its workspace.
- The replacement resolves repository access again and clones from Git.
  Unpushed edits, local-only commits, private environments and outputs are
  lost. The native transcript/usage survives, with an explicit recovery notice
  instructing the model to inspect remote state, recreate edits, reattach
  additional repositories and rerun validation. It cannot return an old cached
  completion result as though the old files still existed.
- Pending tool approval placeholders are invalidated before reconciliation.
  Completed external actions remain recorded; interrupted effects keep their
  unknown-outcome markers. No automatic replay of pushes/messages/API effects.
  Ambiguous legacy execution state still fails closed for review.
- Published artifacts already uploaded through the artifact API survive;
  unpublished local files do not. There are no filesystem checkpoints.
- Runtime-managed repository and analysis workspaces are supported. Retained
  `host_prepared` workspaces must not be moved to this mode.

The shared-filesystem flock is replaced by a PostgreSQL session advisory lock
for each app/run. It uses a dedicated connection while an activity executes;
paused sessions hold no database lock. Losing the connection cancels the
execution context (one-second probe cadence, three-second probe timeout).
Connections are discarded on release so locked sessions cannot leak into the
pool. Use direct PostgreSQL or session pooling, **not transaction pooling**.
Allow connection capacity for a lock connection plus normal queries per active
activity. This is not a claim of exactly-once external side effects: database
failover/network partitions and already-in-flight remote calls can still have
uncertain outcomes, handled by the existing native effect journal.

## Rollout

1. Build matching API and coding-worker images from this branch.
2. Stop admitting new coding work and drain/finish active and paused retained
   runs. Do not mix old and new storage modes on the coding queue. The local
   worker rejects old non-session coding activities rather than silently
   migrating their files.
3. Update image tags and sync the staging overlay through Argo. It includes
   `ephemeral-workspaces` and `node-local-repository-cache`, pins verified
   non-control-plane Landlock ABI 3+ nodes, and sets storage mode on API/worker.
4. Verify a new coding run, pause/resume, pod-loss recovery and published output.
   Monitor free disk, evictions, session capacity and database connections.
5. Only then port the deployment settings to production. Production manifests
   are deliberately unchanged in this branch.

Staging limits: workspace emptyDir 20 GiB, private HOME/tmp emptyDir 20 GiB,
pod ephemeral-storage limit 44 GiB. Kubernetes limits are eviction safeguards,
not synchronous per-file quotas. Local hostPath caches are not charged to that
pod limit; they have separate cache GC and need node free-space monitoring.
Terminal cleanup deletes the current run tree. Abandoned old-session trees on
a surviving pod are reclaimed when the pod is deleted; they remain subject to
the emptyDir/pod disk limits.

Helm opt-in: `codingWorker.workspaceStorage.mode=ephemeral` and suitable
`workspaceStorage.sizeLimit`/pod ephemeral-storage limits. Keep the API and
worker configuration aligned. The old PVC remains rendered (unused), so Argo
or Helm does not prune its data during migration. JuiceFS itself and unrelated
consumers are not deleted. Rollback also requires draining ephemeral runs;
their local data cannot be moved back automatically.

## Verification (2026-09-22)

- Full affected engine/runtime/workspace/durable/worker suites pass; static
  checks, Helm lint and staging Kustomize render pass.
- Linux tools/Landlock suite passes on a disposable staging pod.
- Live PostgreSQL test verifies same-run exclusion, unrelated-run concurrency,
  release/reacquisition and cancellation after terminating only its own lock
  connection. No application records are used by this lock test.
- Live isolated Temporal queue: real workflow, activities, engine, PostgreSQL
  store and Git provider; only the LLM is replaced by a deterministic adapter.
  Deleted the worker pod on node `2090289` during execution. A replacement on
  `2090293` obtained a new session, cloned afresh, verified the lost edit was
  absent, persisted the recovery marker and completed the run. The smoke app/run
  is `ephemeral-recovery-20260922-a`; its audit records remain in staging.
- Unit tests verify approval affinity, stale-session checkout rejection,
  pending-approval invalidation and preservation of external effects/usage.

The local Helpin benchmark uses coding image `v0.12.2-rc.1`, Landlock, copy
imports, two CPU/four GiB limits, a fresh checkout tree, frozen lockfile and
disabled lifecycle scripts. It is online and reuses 2,033 external packages
with zero downloads (2,082 packages total). Lockfile SHA-256:
`e6f8111e2d6ebcec9b8f241066a93453b8127188bb5bc808c87aca4e49f8471c`.

| Phase | Local emptyDir, warm cache | JuiceFS, same warm cache |
| --- | ---: | ---: |
| Install | 18.27 s | 519.63 s |
| Widget build | 4.50 s | 18.03 s |
| Forced TypeScript build | 42.53 s | 70.48 s |
| Focused tests | 2.35 s | 15.54 s |
| Entire benchmark, including fixture copy | 67.85 s | 648.85 s |

The sequential control uses the same pod, node, source, resource limits,
Landlock and copy-import policy, with no overlapping project benchmark.
Both installs reused all 2,033 external packages and downloaded zero. Local
install was approximately 28.4 times faster. During the JuiceFS install, its
client recorded 130,121 block-cache writes (~1.34 GB), ending with 73,643 staged
blocks (~583 MB). This is a single sequential comparison, not a statistical
latency distribution; normal cluster background traffic was not disabled.
All phases passed in both runs. End-to-end improvement was about 9.6 times.

The initial source-fixture copy from JuiceFS exceeded the harness's three-minute
setup timeout. It was completed separately and verified byte-for-byte before
the above measurement. That fixture transfer is not part of install time and
is not a measurement of cloning from Git. Earlier overlapping JuiceFS runs
took 534/539 seconds for install; the sequential same-pod control above
reproduces the slow install without that overlap.

Normal staging deployments have not been changed by these smoke tests. The
manifest opt-in must ship with matching new images, not the currently pinned
old image. The `--ephemeral-workspaces` worker flag makes an old image fail
startup instead of silently ignoring session/recovery settings. Nothing has
been deployed to production.

The two recovery pods and the completed comparison pod are removed after
verification. The earlier `runtime-local-cache-a2` test pod remains temporarily
to keep the JuiceFS client mounted while the comparison's remaining staged
blocks drain (62,527 blocks / ~501 MB at benchmark completion). It has a
two-hour lifetime. No benchmark command remains running. Logs are saved under
`/tmp/ephemeral-workspace-results-20260922` on the staging bastion and copied
locally to `/private/tmp/node-local-cache.yZObSg/`.
