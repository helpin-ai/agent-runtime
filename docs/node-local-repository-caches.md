# Node-local repository caches

Repository mode now uses local disk, not the JuiceFS workspace volume. The
operator supplies `AGENT_RUNTIME_REPOSITORY_CACHE_ROOT`; cache keys still use
trusted app/workspace/repository identity plus namespace and OS/architecture.
Runs and pods on the same node reuse the same cache. A pod scheduled elsewhere
downloads its own dependencies. Node/disk loss makes that node's cache cold.

Shared caches cover npm/pnpm/Yarn, pip/uv/Poetry downloads, Go modules/builds,
and Cargo registry/Git dependencies. Private Python/Poetry environments, Cargo
configuration/credentials/target, checkouts and outputs stay private to the run.
The staging manifests now select disk-backed `emptyDir`, not JuiceFS; see
[ephemeral workspaces](ephemeral-workspaces.md) for recovery and rollout.
HOME and temporary files stay in pod-local scratch. Normal runs remain online.
No filesystem checkpointing, cache uploads or cross-node cache reads exist.

Landlock grants only the authorized repository cache, run and private scratch.
Different runs sharing a repo are one cache trust domain: this is not protection
against cache poisoning by another authorized writer. Copy imports avoid shared
hardlink mutations. Native package-manager locks remain, and worker usage locks
protect active caches against background eviction.

## Deployment

- Staging opts into `k8s/components/node-local-repository-cache`, using
  `/var/lib/agent-runtime/repository-cache` on the host, mounted at
  `/var/cache/agent-runtime-repositories`. Only the two verified FSN1 AX41 workers
  are eligible. Strict Landlock ABI 3+ (including truncate protection) is
  required; no control-plane placement.
- A root init container without privileged mode, with only CHOWN/FOWNER, changes ownership
  and mode of the dedicated mount root, never recursively touching contents.
  The worker and all agent commands run as UID/GID 1000.
- Helm defaults to private ephemeral caches and retained checkouts. Opt into
  shared node-local caches with `codingWorker.repositoryCache.mode=repository`
  and `codingWorker.isolation=landlock`; configure `repositoryCache.hostPath`
  and `mountPath` if needed. Keep storage roots outside broad sandbox read grants.
  For init-container safety, Helm requires the host path to end with
  `/agent-runtime/repository-cache` and rejects traversal or broad host roots.
- The host directory must be on a real local SSD/NVMe filesystem. Provision a
  dedicated disk/partition for capacity isolation where possible. A hostPath is
  not bounded by the pod's ephemeral-storage limit. Monitor node free space;
  20 GiB is a **soft per-repository** budget, not an aggregate hard disk quota.
- Idle expiry remains seven days. Maintenance runs against the local cache root
  only. Existing experimental JuiceFS caches are neither reused nor deleted.
- Production manifests are unchanged. Do not sync the new staging configuration
  with an old runtime image: merge/release the code and matching image first.

The private-cache recovery option remains available for corruption; it uses
private per-run workspace caches, not the shared local store. With ephemeral
workspaces this fallback also uses local disk.

## Verification

Unit tests cover same-node cross-run/restart reuse, independent cold node roots,
trusted identity isolation, no shared cache on the workspace volume, invalid
root rejection, usage locks and eviction. Linux staging smoke tests must set
`AGENT_RUNTIME_REPOSITORY_CACHE_ROOT` to the local mount in addition to their
disposable workspace fixture. Run seed/reuse in separate pods on the **same**
node; use a second node for an independent cold seed. Lease hold/check phases
must run against the same node-local disk, not different nodes.

The [older measurements](workspace-cache-rollout.md) are historical and are not
performance claims for this implementation. The measurements below predate
ephemeral checkouts; the newer comparison is in the ephemeral-workspaces guide.

### Staging results, 2026-09-22

- Actual cache mount: ext4 `/dev/md1`, separate from `fuse.juicefs` workspaces.
- Full workspace, worker and tools package suites pass on Linux staging.
  Focused cache race checks and `go vet` pass; Helm and Kustomize render.
  The actual JuiceFS mount is rejected by the Linux cache-filesystem guard.
- Cold seed on node A passed for pnpm/npm/Yarn/Poetry/Go/Cargo/pip/uv.
  A different pod on the same node reused it with pnpm/npm/Yarn/Go/Cargo/uv
  offline checks and pip's cached wheel. Poetry remained online. Landlock denied
  another workspace's cache, sibling run state and cache-control writes.
- Node B started with an empty local cache and downloaded dependencies itself;
  its independent cold seed passed. No cache bytes were fetched from node A.
- Deleting/recreating a test pod on node A preserved the local cache. A new
  normal online run passed with pnpm reusing its package, pip using its cached
  wheel and Go using its build cache. Online Cargo still checks the remote Git
  dependency; private Rust targets rebuild as designed.
- Concurrent processes in two same-node pods passed the lease/GC test: active
  caches survived eviction attempts and were removed only after release.

Full Helpin fixture: 2,082 packages, coding image `v0.12.2-rc.1`, two CPU/four
GiB limits per test pod. Cold install passed in 534.29 seconds, widget build in
16.75 seconds, TypeScript in 65.65 seconds and focused tests in 13.97 seconds
(659.54 seconds total including fixture setup). A second run in
another same-node pod reported 2,033 reused packages and zero downloads while
creating its own dependency tree. These overlapping runs are a correctness/load
check, not a controlled speed comparison. Their private dependency trees still
generate substantial JuiceFS writes and pending uploads; the local dependency
cache itself does not.

The second full run also passed: install 538.91 seconds, widget build 16.37
seconds, TypeScript passed, focused tests 14.17 seconds; 657.41 seconds total
including fixture setup. It reused all 2,033 external packages with zero
downloads. This does **not** demonstrate a faster complete install than the cold
run; private dependency-tree writes on JuiceFS remain expensive.

Regular staging workers have not been redeployed and production is unchanged.
At the end of these initial cache tests, two completed pods were removed and
the final test pod was retained temporarily
(7,200-second lifetime) to keep the JuiceFS client mounted while approximately
139,738 pending workspace blocks drained. Disposable local caches and workspace
fixtures were retained for the subsequent ephemeral-workspace comparison.

The full native tool suite is Linux-specific: a direct macOS run hits existing
`/proc` and `/var`-symlink assumptions. Worker macOS CGO linking also hits the
installed SDK/toolchain mismatch; pure-Go focused worker tests pass.
