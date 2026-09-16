# CI and releases

Pull requests to `develop` and `main` run Go tests (including PostgreSQL), React
tests, the console build, manifest validation, and the Linux CLI tests, race
check, build, and OAuth smoke test. New PR revisions cancel obsolete checks.
PRs do not build Docker images or upload CLI packages.

Pushes to `develop` and `main` run the staging and production release workflows,
respectively. Each release calls the same CI and CLI checks for its commit,
and packages the CLI for Linux amd64/arm64 and macOS amd64/arm64. The standalone
CI and CLI workflows do not also run on pushes. Markdown-only and Kubernetes-only
pushes retain the existing release exclusions. Manual releases must be dispatched
on the matching branch.

After checks pass, both release workflows call `release-images.yml`. It builds
the support, coding, and console images once, using persistent BuildKit caches.
Support and coding smoke tests run against the built images before any image
is pushed. The console's Docker build includes its build and type check, so a
release skips the separate console build job. Publishing tags these same local
images with the version, commit SHA, and environment alias; it does not rebuild
them. Release tags point to the tested commit even if the branch has advanced.

Cross-platform CLI binaries are downloadable from the release workflow's Actions
artifacts. Packaging and Docker failures therefore block release publication;
they are no longer PR gates. Tests intentionally run again on the merged commit
to cover merge changes and direct pushes.

Go jobs and the runtime Dockerfile prefetch dependencies through
`scripts/go-mod-download.sh`. Failed downloads get up to three attempts with
5- and 10-second backoff. The last attempt disables HTTP/2 for that download
process to handle proxy stream failures. Permanent failures still fail the job;
the configured module proxy, dependency versions, and checksum verification
remain in effect.

If branch protection requires the former `Container Builds` or cross-build
checks, remove those requirements when adopting these workflows. Keep the Go,
React, Console, Manifest, and CLI checks required. Repository settings are not
managed by these files.
