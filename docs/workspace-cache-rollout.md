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
2 CPU / 4 GiB disposable workers. Runs start with fresh checkouts and copy
imports. Normal agent runs are online; offline checks below are diagnostics
that distinguish cache availability from fallback downloads, not the rollout gate.

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

A fresh offline retry after the queues drained completed installation in
**472.22 s** with 2,033 reused packages and zero downloads.
Its subsequent widget/typecheck/test phases passed in **129.62 / 437.54 /
277.90 s**. These overlapped later storage load and incurred cold client reads;
they are not evidence of faster validation. Package reuse does not eliminate
fresh dependency-tree metadata work or cross-node read latency.
It cannot establish safety while new concurrent cache writes are still pending,
nor does it represent a cold-client benchmark because the failed attempt warmed
the reader's client cache.

JuiceFS documents this limitation in its
[cache guide](https://juicefs.com/docs/community/guide/cache/) and
[writeback explanation](https://juicefs.com/en/blog/solutions/juicefs-write-acceleration).
Larger read caches or persistent staging disks do not remove the visibility gap.

### Normal online runs with overlapping cache population

Fresh online seed and reuse runs were started on different nodes against a new
shared cache. The writer had **47,205 staged blocks / 423,547,671 bytes** while
the reader was active. The online seed passed: install **711.62 s**, widget
**16.39 s**, forced TypeScript **63.77 s**, focused tests **14.77 s**.

The online reader **timed out at 900.12 s**, with **1,631 packages reused,
398 downloaded and 181 added**. It logged `ERR_PNPM_EIO` retries despite network
access. This is a failed online stress test, not a performance improvement.

One explicit private-cache retry of the **same partially installed checkout**
completed installation in **303.52 s**. It downloaded 2,033 packages into private
workspace cache state and did not clear the shared store. This is recovery,
not a clean-room speed comparison: it reused the checkout, ran after the first
writer finished, and private imports may safely hardlink within the same run
whereas shared imports copy. The original failed attempt's 900 s still counts
toward the total user wait. The private retry's widget build, forced TypeScript
and focused tests all passed in **13.42 / 56.53 / 11.07 s**, respectively;
the complete recovery test took **384.64 s**.

The small online npm, pnpm and Yarn checks passed. Poetry failed after **106.84 s**
with a generic PyPI connection error; verbose retry exposed **`[Errno 5]
Input/output error`**, while a direct PyPI request returned HTTP 200. This proves
normal online mode does not automatically recover every pending-upload cache
read failure. The original all-toolchain sequence stopped at Poetry; later
Go/Rust/pip/uv steps in that original sequence were not executed. A separate
online sequence later passed them: Go **21.50 s**, Rust **23.94 s**, pip cached
wheel **1.12 s**, uv **1.75 s**. That later pass does not prove recovery from an
unreadable cache for those managers.

An explicit `run_command` **`private_cache: true`** recovery option now bypasses
the shared cache without deleting it, changing authorization bindings, or
automatically replaying commands. Package-manager failures explain how to retry
once if the error is cache-related. The private path is Landlock-confined and
does not require a shared-cache usage lock or grant. Normal commands still use
the global cache by default. This is a recovery control, not a promise that cold
private installs or fresh dependency trees are instantaneous.

The failed Poetry checkout recovered through the actual `run_command` path.
The final check installed into its original private environment in **0.80 s**
and verified a subsequent normal command could import the dependency. A first
private-cache attempt took **6.34 s** to populate the private download cache;
the final 0.80 s result therefore is not a cold-cache timing.

The live mount is already configured with **50 concurrent uploads**, a
**512 MiB buffer**, and **zero upload delay**. A reader-side sample during the
private retry showed all 50 upload slots occupied, with cumulative object-store
means of approximately **128 ms/GET** and **353 ms/PUT** (not isolated per-test
measurements). The problem is not an accidentally disabled upload queue.

## Reproduction and deployment

`internal/tools/repository_cache_linux_test.go` runs with:
`AGENT_RUNTIME_TEST_WORKER_BINARY`, `AGENT_RUNTIME_CACHE_SMOKE_ROOT`,
`AGENT_RUNTIME_CACHE_SMOKE_PHASE=seed|reuse`, and optionally
`AGENT_RUNTIME_CACHE_SMOKE_RUN` for simultaneous distinct runs.
Set `AGENT_RUNTIME_CACHE_SMOKE_ONLINE=1` for normal online reuse and
`AGENT_RUNTIME_CACHE_SMOKE_EXTRA=1` to include npm, Yarn and Poetry.
Run seed on one pod and reuse on another. Seed intentionally removes its own
checkout at completion. The lease test uses `AGENT_RUNTIME_CACHE_LEASE_ROOT`
and simultaneous phases `hold|check`. All paths must be disposable test fixtures.

`TestRepositoryCacheProjectStaging` additionally takes
`AGENT_RUNTIME_CACHE_PROJECT_SOURCE` for the pristine project fixture. It is
online by default; `AGENT_RUNTIME_CACHE_PROJECT_OFFLINE=1` is an explicit
diagnostic only. Start reuse while seed is still populating the cache and record
the writer's `.stats` backlog alongside both test logs. Backlog counters in each
command result describe that command's own mount, not the remote writer.
For one private retry of a failed disposable project checkout, set
`AGENT_RUNTIME_CACHE_PROJECT_PRIVATE=1` and
`AGENT_RUNTIME_CACHE_PROJECT_RESUME=1` with the same run ID. Never point this
test at a live user checkout. The targeted Poetry recovery test instead accepts
`AGENT_RUNTIME_CACHE_RECOVERY_ROOT` and exercises the real tool input and a
subsequent normal command.

Staging JuiceFS already uses a 10 GiB client cache and a 10% free-space reserve
(manifests commit `bc9f7ee`). This is separate from repository-cache retention.
A larger client cache did not eliminate cold object-store latency.

The chosen design remains a globally shared workspace/repository cache with
JuiceFS writeback and normal online package-manager behavior. No node-specific
cache partition, write-through mount, checkpointing or snapshot system is added.
The rollout gate is successful online execution under pending uploads, including
measured fallback/error behavior; an offline-only failure does not reject this
best-effort design. Never assume every filesystem I/O error is automatically
treated as a cache miss by every package manager.

Regular workers have not received this runtime image yet. **Cross-run sharing
is not enabled by the staging overlay:** it remains on `workspace`. The large
online overlap test failed its time budget, so the recovery option is not being
presented as a passed performance rollout. The implementation remains opt-in
for review and controlled experiments. After the online validation gate passes,
merge into develop, let the release pipeline
publish/update the image, sync through Argo CD, and verify a real new coding
run uses the shared key.
Production is unchanged. Roll back with mode `workspace` or `ephemeral`;
existing shared cache data is left intact, and shared-cache maintenance stops.

Disposable verification pods have 7,200-second lifetime limits. They were left
running after the tests to drain pending writeback data (the reader still had
62,269 staged blocks at the end of recovery), rather than deleting its mount
while uploads were pending. Benchmark fixtures are retained for investigation.
