# Cross-run repository caches — staging rollout

## Behavior

`AGENT_RUNTIME_REPOSITORY_CACHE_MODE=repository` shares dependency caches across
different coding runs authorized for the same app, Helpin workspace and repository.
The host's freshly resolved repository spec supplies `workspace_id` and
`repository_id`; run input, checkout files and persisted cache paths cannot
authorize sharing. Branch names and run IDs are excluded from cache identity.

The worker records that binding in memory when preparing/validating the checkout.
Cache data lives outside run directories at
`<workspace-volume>/.repository-caches/<sha256>`. The key includes the cache
namespace and OS/architecture. Missing trusted identity falls back to private
workspace caches. Run cleanup never removes shared caches.

| State | Location/lifetime |
| --- | --- |
| npm cache; pnpm package store **and resolution metadata**; Yarn cache | Shared repository cache |
| pip, uv, Poetry downloads/wheels | Shared repository cache |
| Go module/build caches | Shared repository cache |
| Cargo Git/registry dependencies and package-cache locks | Shared repository cache |
| Python venvs; Poetry environments | Private persistent workspace |
| Cargo home configuration/credentials; Cargo target/build outputs | Private persistent workspace |
| HOME and temporary files | Private ephemeral state |

Cargo target directories are deliberately not shared: they contain mutable
runnable outputs and are not a general content-addressed compilation cache.
Different branches must not overwrite each other's binaries. Rust dependencies
are reused, but compilation still runs. No checkpointing or workspace snapshots
were added.

pnpm and uv default to copy imports; pnpm store integrity checks remain enabled.
Agent commands can override tool settings within their authorized trust domain.
Cache sharing is explicitly a **workspace/repository trust boundary**: malicious
writers can poison other runs' cache inputs within that boundary. Integrity
checks and copy imports do not make a shared writable store mutually untrusted.
Landlock exposes only the authorized cache root, the run, and private scratch;
other cache roots, private checkouts, and cache control files remain denied.
These isolation claims require enforced Landlock, not an unsupported-kernel
best-effort fallback.

## Lifecycle and limits

- Default mode remains `ephemeral`; `workspace` retains the earlier per-run mode.
- `AGENT_RUNTIME_REPOSITORY_CACHE_NAMESPACE=v1`: bump for incompatible toolchain
  or cache-policy changes. Toolchains additionally apply their own cache keys.
- `AGENT_RUNTIME_REPOSITORY_CACHE_TTL=168h`: idle expiry.
- `AGENT_RUNTIME_REPOSITORY_CACHE_MAX_BYTES=21474836480`: soft 20 GiB budget per
  repository/generation, not a hard quota. Zero disables either limit.
- Each command holds a shared, cross-worker usage lease through execution.
  Package managers retain their native locks; whole coding runs are not serialized.
- Background maintenance checks every ten minutes with a bounded scan budget.
  It skips active caches, sizes outside the exclusive fence, then rechecks last
  use before eviction. Large scans may defer size enforcement.
- Eviction atomically retires an idle cache before deleting it, so a new command
  can create a cold cache without waiting for remote tree deletion. Later
  maintenance handles interrupted retirement. Control lock files stay stable.
- No credentials, agent authorization records, source checkouts or results are
  promoted into shared cache state by the runtime.

## Verification

The full Linux `go test ./... -count=1` suite, focused race checks, `go vet`,
Helm lint/rendering and staging Kustomize rendering pass. Unit tests cover
cross-run/branch reuse, app/workspace/repository separation, forged input,
namespace changes, missing-identity fallback, symlink attacks, cleanup, and
idle TTL/size eviction without deleting active or unrecognized directories.

Staging tests use disposable non-root pods with the normal coding image
`v0.12.1-rc.1`, no application credentials and the new cross-compiled runtime
test code. Both nodes enforce Landlock:

- AX41 FSN1 DC15 `2090289`
- AX41 FSN1 DC15 `2090293`

A seed run populated pinned Node, Go, Python and Rust dependencies and deleted
its entire checkout/private environments. A new run on the other node reused
pnpm, uv, Go and Rust dependencies with downloads disabled where supported;
pip reported its cached wheel while installing into a fresh private venv.

An initial offline test exposed pnpm's separate resolution-metadata cache; this
was fixed and the seed/reuse sequence rerun. The tests assert Node dependency
link count is one and Landlock denies another tenant's cache, sibling checkouts
and cache-control writes.

Four distinct runs then passed concurrently across both nodes:
pnpm offline installs 0.58–0.70 s, Go builds 1.19–2.11 s, private Rust builds
15.69–23.76 s, pip cached installs 0.92–15.43 s, uv offline installs 0.26–3.94 s.
These are tiny fixtures, not performance guarantees. Native Cargo package-cache
lock waits were observed. Cold-node cache reads can be slower than recomputing:
the first cross-node Go build took 21.24 s versus the seed's 17.44 s.

A separate two-node test proved JuiceFS usage leases prevent active-cache
eviction and allow eviction after release. It is not just a local-process test.

Additional fresh-run checks passed for npm and Yarn offline installs (0.87 s
and 0.47 s), and Poetry installation into a new private environment (13.56 s).
Poetry was not forced offline, so its check proves the environment/cache wiring,
not zero network usage.

### Full Helpin project benchmark

`TestRepositoryCacheProjectStaging` uses pristine Helpin commit
`fcfadfa62d5c1671e69b258c2a4e70a6a083e8a1` (16 projects, 2,082 packages), with
2 CPU / 4 GiB disposable workers. Both runs start with a fresh checkout; the
second uses the first run's cache, copy imports and offline installation.

The seed run passed: install **717.07 s**, widget build **27.80 s**, forced
TypeScript **60.53 s**, focused tests **14.15 s**. The first cross-node reuse
**failed after 513.07 s** with `ERR_PNPM_NO_OFFLINE_TARBALL` for
`@noble/curves@1.9.7`: 2,026 packages reused, zero downloaded, 2,075 added.
These are not a successful cold/warm comparison.

The reader's JuiceFS mount logged repeated R2 `404 NoSuchKey` errors while the
seed client's writeback upload queue was still draining (tens of thousands of
small blocks). After drain, the reported missing package's index and every
content checksum were readable and correct on both clients. This reproduces
the documented client-writeback visibility gap, not a Landlock permission denial.
Usage leases prevent cache eviction; they do not make pending blocks readable
on another client. Normal online installs may redownload unavailable packages,
but that is not a consistency guarantee for all shared package caches.

A fresh offline retry after the queues drained is being recorded separately.
It cannot establish safety while new concurrent cache writes are still pending,
nor does it represent a cold-client benchmark because the failed attempt warmed
the reader's client cache.

JuiceFS documents this limitation in its
[cache guide](https://juicefs.com/docs/community/guide/cache/) and
[writeback explanation](https://juicefs.com/en/blog/solutions/juicefs-write-acceleration).
Larger read caches or persistent staging disks do not remove the visibility gap.

## Reproduction and deployment

`internal/tools/repository_cache_linux_test.go` runs with:
`AGENT_RUNTIME_TEST_WORKER_BINARY`, `AGENT_RUNTIME_CACHE_SMOKE_ROOT`,
`AGENT_RUNTIME_CACHE_SMOKE_PHASE=seed|reuse`, and optionally
`AGENT_RUNTIME_CACHE_SMOKE_RUN` for simultaneous distinct runs.
Run seed on one pod and reuse on another. Seed intentionally removes its own
checkout at completion. The lease test uses `AGENT_RUNTIME_CACHE_LEASE_ROOT`
and simultaneous phases `hold|check`. All paths must be disposable test fixtures.

Staging JuiceFS already uses a 10 GiB client cache and a 10% free-space reserve
(manifests commit `bc9f7ee`). This is separate from repository-cache retention.
A larger client cache did not eliminate cold object-store latency.

Regular workers have not received this runtime image yet. **Do not enable
`repository` on the current multi-node writeback mount.** The staging overlay
remains `workspace`. A separate write-through mount for shared cache data would
retain workspace writeback without exposing incomplete shared-cache blocks;
however, it adds synchronous cold-fill latency and is not being deployed.
A lower-complexity alternative is a node-specific cache namespace: different
runs on the same node reuse caches through that node's shared CSI mount, while
other nodes warm separate copies. That trades cluster-wide hits for avoiding
cross-client reads of pending uploads. It requires verifying that workers on
each node really share one mount client, and it retains writeback's disk-loss
risk. Neither alternative is enabled by this change yet.
After the storage gate passes, merge into develop, let the release pipeline
publish/update the image, sync through Argo CD, and verify a real new coding
run uses the shared key.
Production is unchanged. Roll back with mode `workspace` or `ephemeral`;
existing shared cache data is left intact, and shared-cache maintenance stops.
