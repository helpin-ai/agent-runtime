# CI and releases

Pull requests to `develop` and `main` run Go tests (including PostgreSQL), React
tests, the console build, and manifest validation. CLI unit tests remain covered
by `go test ./...`. New PR revisions cancel obsolete checks.
PRs do not build Docker images or upload CLI packages.

Node builds use the version in `.node-version`, also pinned by digest in the
Dockerfiles. JavaScript actions use Node 24, with reviewed commit SHA pins and
weekly Dependabot updates. Any replacement self-hosted runners must be version
2.329.0 or newer. Workflow tokens default to read-only; only image publishing and
Git release jobs receive write permissions. Image smoke tests verify the Node
version and browser binary. The pinned browser package alone is allowed to run
its npm postinstall script when building the runtime images.

Pushes to `develop` and `main` run the staging and production release workflows,
respectively. Each release calls the same CI checks for its commit. The standalone
CI workflow does not also run on pushes. Markdown-only and Kubernetes-only
pushes retain the existing release exclusions. Manual releases must be dispatched
on the matching branch.

After checks pass, both release workflows call `release-images.yml`. Support,
coding, and console each build on a separate runner through
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
packaging. Docker failures still block publication. Tests intentionally run again
on the merged commit to cover merge changes and direct pushes.

Go jobs and the runtime Dockerfile prefetch dependencies through
`scripts/go-mod-download.sh`. Failed downloads get up to three attempts with
5- and 10-second backoff. The last attempt disables HTTP/2 for that download
process to handle proxy stream failures. Permanent failures still fail the job;
the configured module proxy, dependency versions, and checksum verification
remain in effect.

If branch protection requires the former `Container Builds`, dedicated CLI, or cross-build
checks, remove those requirements when adopting these workflows. Keep the Go,
React, Console, and Manifest checks required. Repository settings are not
managed by these files.
