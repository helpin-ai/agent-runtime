# Workspace cache rollout — staging, 2026-09-22

## Scope of this increment

Opt-in `AGENT_RUNTIME_REPOSITORY_CACHE_MODE=workspace` stores regenerable
toolchain state at `<run>/repositories/<fingerprint>/tool-cache/<namespace>-<os>-<arch>`.
Repository identity and run ownership come from the existing checkout layout.
No extra writable Landlock root is granted. Directory preparation uses `os.Root`
to reject escaping symlinks. HOME and temporary files stay pod-local.

Configured persistent caches:

| Toolchain | State |
| --- | --- |
| Node | npm cache, pnpm store, Yarn cache |
| Python | pip, uv, Poetry caches; interpreter-keyed private Python venv |
| Go | module cache, build cache, GOPATH |
| Rust | Cargo home (registry/Git dependencies), target directory |

The default remains `ephemeral`; only the staging overlay opts in. Analysis
scratch retains its previous lifetime. Existing cleanup removes caches together
with terminally deleted workspaces; manually retained workspaces keep them.
This is same-workspace reuse across pod replacement, not cross-run repository
sharing. There is no independent cache TTL/quota or checkpointing in this change.

## Verification

The full Linux `go test ./... -count=1` suite passed. Focused `go vet`, Helm
lint/rendering, staging Kustomize rendering, private cache separation, cleanup,
namespace switching, Python restart survival, and symlink-rejection tests passed.
Linux containers were used because the developer Mac's procfs/path assumptions
and Command Line Tools linker prevented a representative full native run.

The new code was cross-compiled and executed in a disposable staging pod using
the deployed coding image (`v0.12.1-rc.1`) and its normal non-root/seccomp security
context, without application credentials. Landlock was enforced on the AX41
kernel-6.8 node. The pod was deleted and recreated with the same RWX PVC and a
fresh emptyDir. The test binary was copied back; dependencies were not.

| Test | Initial seed | Replacement pod |
| --- | ---: | ---: |
| pnpm, one pinned package | 2.526 s | 0.590 s, offline, one reused, zero downloads |
| Go, pinned UUID dependency | 15.428 s | 0.836 s, GOPROXY/GOSUMDB disabled |
| Rust, pinned itoa Git dependency | 29.060 s | 19.510 s, offline; dependency reused, fixture rebuilt |
| Python, pinned six dependency | 1.074 s install | 0.070 s retained-environment import |

These are small smoke fixtures, not full-repository performance guarantees.
The Rust fixture rewrites source on each phase, triggering a local crate rebuild.
Cargo 1.65's whole-registry Git index was avoided by pinning a small Git dependency.
The real Rust Git/build caches were exercised; registry, npm, Yarn, uv, and Poetry
cache paths were checked, but their individual package-download flows were not
all benchmarked. Landlock sibling-read/write denial and an escaping cache symlink
test passed on the actual staging kernel.

Reproduce using `internal/tools/repository_cache_linux_test.go`:

```sh
AGENT_RUNTIME_TEST_WORKER_BINARY=/path/to/matching/agent-runtime-worker \
AGENT_RUNTIME_CACHE_SMOKE_ROOT=/path/on/disposable/shared/volume \
AGENT_RUNTIME_CACHE_SMOKE_PHASE=seed \
./tools.test -test.run '^TestRepositoryCacheStaging$' -test.v
# Replace the pod, restore only the binaries, repeat with phase=reuse.
```

## JuiceFS observations

Staging manifests commit `bc9f7ee` enlarged the client cache from 1 GiB to 10 GiB
and reduced free-space-ratio from 0.30 to 0.10. The existing PV was patched and
coding workers rolled so both actual mount commands use those values.

With 10 GiB configured, a first forced frontend typecheck timed out at 300.076 s;
its repeat passed in 111.034 s. Focused tests passed in 160.182 s cold and 10.550 s
warm. The warm test had zero cache misses; all measured follow-up commands had
zero evictions. Cold-read latency remains substantial. These are not controlled
cache-size A/B results: mount/page-cache state changed, no caches were flushed,
and the forced typecheck differs from the earlier non-forced baseline.

The read-only shared pnpm store experiment failed immediately because pnpm
registers a project symlink inside its store. Do not resolve this by granting
arbitrary runs write access to a common store: imported hardlinks can alias
writable cache inodes. Cross-run reuse needs a separately validated preparation
or ownership mechanism; it is not silently enabled here.

## Deployment gate and next steps

The regular staging workers have not yet received this runtime implementation.
The release workflow only builds from `develop`; merge the feature branch, let
CI publish/update the staging image, and sync through Argo CD. Then verify the
cache-mode environment variable and repeat a real coding-session continuation
across worker replacement. Do not promote to production before that check.

Further work remains: safe reuse across independent runs for the same repository,
bounded retention of manually retained caches, and full-project warm validation
measurements. Keep these distinct from the proven same-workspace restart reuse.
No production manifests or deployments were changed for this increment.
