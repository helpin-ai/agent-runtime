# CI and releases

Pull requests to `develop` and `main` select affected Go tests (including
PostgreSQL), React tests, console build, release-script tests, Windows cross-builds,
and native Windows checks. Console-only and documentation-only PRs do not start
Windows runners. Dependency, validation-script, and workflow changes retain broad
coverage. Unknown source areas conservatively select Go and Windows checks.
CLI unit tests remain covered by `go test ./...`. New PR revisions cancel obsolete
CI and Windows checks. PRs do not build Docker images or upload CLI packages.

Successful checks produce small `verified-ci-<tree>` and
`verified-windows-<tree>` artifacts. The selector looks up at most 20 records for
the exact Git tree, including workflows, dependencies, and build scripts. It
validates the successful originating workflow, repository, branch/event, run
attempt, source commit/tree, and individual checks before skipping work. PR
records must come from this repository and target `develop` or `main`; fork
records are never reused. Synthetic PR merge commits may differ from the run's
head SHA, but must contain that head as a parent and have the exact tested tree.
Records expire after 30 days. Missing, invalid, expired, failed, or inaccessible
validation falls back to testing; a changed merge or direct push cannot inherit
checks from a different tree. API failures also fall back to testing.

The final `CI validation` and `Windows validation` jobs fail if a required check
failed, was cancelled, or was unexpectedly skipped. They only certify checks that
actually passed or were verified against an earlier successful record. Checks
omitted because their source area did not change are not certified for reuse.

Linux runs the full shared Go suite once, including the JavaScript extension
tests, Python fixtures, and Landlock tests. It also cross-compiles the CGO-free
Windows x64 and ARM64 runtime. One Windows job covers native locks, ACLs, Windows
paths, workspace ownership, memory-worker restart/ownership, checkpoint restore,
SQLite migration/persistence, process-tree cleanup, and the real Chrome extension
fixtures (including keyboard and frames). Windows uses a different SQLite driver,
so its focused persistence tests remain necessary. Browser setup and Go compilation
are shared with the native checks. The real GUI fixtures remain on Windows;
Linux GUI execution is not assumed. WSL2 confinement proof remains a separate
manual workflow, with the existing path-filtered Windows-support branch trigger.

Node builds use the version in `.node-version`, also pinned by digest in the
Dockerfiles. JavaScript actions use Node 24, with reviewed commit SHA pins and
weekly Dependabot updates. Any replacement self-hosted runners must be version
2.329.0 or newer. Workflow tokens default to read-only; only image publishing and
Git release jobs receive write permissions. Image smoke tests verify the Node
version and browser binary. The pinned browser package alone is allowed to run
its npm postinstall script when building the runtime images.

Pushes to `develop` and `main` run the staging and production release workflows,
respectively. Each release resolves existing CI validation before running missing
checks. Unlike PR path selection, releases and direct pushes require every shared
check unless the exact tree was already validated; the console is validated by
its release image build. The standalone CI workflow does not also run on pushes.
Markdown-only pushes retain the existing release exclusions. Manual releases must
be dispatched on the matching branch. Tags do not start another build workflow.

After checks pass, both release workflows call `release-images.yml`. Production
first looks for a successful staging run with the same Git source tree, including
the Dockerfiles, workflows, and build scripts. Merge commit SHAs may differ. A
matching `linux/amd64` build reuses its three smoke-tested image digests, skipping
the image builds and repeated smoke tests. CI validation is independently reused
when available; otherwise the missing checks run before image promotion.

Staging records the source commit/tree, platform, run/attempt, and three tested
digests in the `tested-release-images` Actions artifact after promotion succeeds.
Production validates that record against the successful staging run and checks
that every digest still exists in GHCR; it never resolves `stage-latest`.
Records are retained for 30 days, and lookup considers the latest 100 successful
staging runs. A missing, expired, invalid, or unavailable record or image falls
back to building and smoke-testing all three images. The first release after
adopting this workflow needs a staging run that produces the record.

When builds are required, support, coding, and console each build on a separate runner through
`release-image-build.yml`, using persistent BuildKit caches. Builds push directly
to GHCR under `candidate-<run-id>-<attempt>` tags, without loading an export
tarball into the build runner's Docker daemon. This avoids storing BuildKit's
cache, an image export, and Docker's unpacked image together on the same disk.

Three fresh runners pull the build outputs by digest and smoke-test the support,
coding, and console images. The console test starts its HTTP server and checks
`/api/health`; its Docker build already includes its build and type check.
Only when every smoke test passes does the promotion job copy those exact
registry manifests to the version, commit SHA, and environment alias tags.
Promotion does not rebuild or download image layers. Versioned tags are published
before aliases; cross-image registry updates are not transactional, so deployment
manifest updates remain gated on the entire release-images workflow succeeding.
Release tags point to the tested commit even if the branch has advanced.

Failed candidates can remain in GHCR but are never selected by deployment tags.
Candidate cleanup is not automatic; remove only old candidates that are not also
referenced by release tags. Keep candidates long enough for failed-job retries.
Run/attempt-qualified names avoid collisions; passing digests through job outputs
preserves identity even when only failed jobs rerun.

Dedicated CLI CI is temporarily paused: its PR/reusable triggers, release calls,
and release dependencies are commented out. The complete `Local CLI` workflow
remains available through `workflow_dispatch` for manual builds, smoke and race
checks, and Linux/macOS amd64/arm64 artifacts. Uncomment all three integration
points when CLI release work resumes. Runtime releases do not wait for CLI
packaging. Docker failures still block publication. Merged commits with a different
source tree and unverified direct pushes still run checks.

Go jobs and the runtime Dockerfile prefetch dependencies through
`scripts/go-mod-download.sh`. Failed downloads get up to three attempts with
5- and 10-second backoff. The last attempt disables HTTP/2 for that download
process to handle proxy stream failures. Permanent failures still fail the job;
the configured module proxy, dependency versions, and checksum verification
remain in effect.

Use `CI validation` and `Windows validation` as the required PR checks. If branch
protection still requires the removed `Real Windows extension browser`,
`Linux command and isolation regressions`, former `Container Builds`, or dedicated
CLI jobs, remove those requirements. The Go, React, Console, and Manifest job names
remain available but may be skipped when unaffected or already verified. Repository
settings are not managed by these files.
