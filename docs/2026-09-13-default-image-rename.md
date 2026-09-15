**Rename the "support" image, worker, and queue wording to "default".**

Date: 2026-09-13. Mechanical rename, no behavior change, agent-runtime only. Helpin has no references.

**Why.** The image and workers that lack the compiler toolchain serve every agent that does not need a shell: Ask Agent, Epic Planner, Documentation Agent, CRM Operator, Marketer, Task Planner, and the support agent. Calling them "support" came from the strategy doc's "support-only image" phrase and misdescribes what they run. The queues they poll are already named native-interactive, native-autonomous, and automation, so "default" matches the code. "Coding" stays as the name of the isolated image, worker, and queue.

**Vocabulary after the change.**

| Term | Meaning |
| --- | --- |
| default image | published as `agent-runtime`, no compilers, cannot serve the coding queue |
| default workers | poll native-interactive, native-autonomous, and automation |
| coding image | published as `agent-runtime-coding`, compilers included |
| coding worker | one replica with a retained volume, polls only native-coding |

Published image names, queue names, flags, environment variables, Helm values, and Kubernetes manifests do not change. The Dockerfile's default target keeps building the same stage under its new name.

**Changes, in one commit.**

| File | Change |
| --- | --- |
| `Dockerfile:72` | stage `AS support` becomes `AS default` |
| `scripts/container-support-smoke.sh` | rename to `container-default-smoke.sh`; usage line and the "support worker accepted coding" message |
| `.github/workflows/ci.yml:117` | step name and script path |
| `README.md:79` | script path and surrounding wording |
| `cmd/agent-runtime-worker/main.go:34,41` | comment and the startup error: "the default image cannot serve coding; use the coding image" |
| `internal/durable/queues.go:33` | comment |
| `internal/engine/execution_policy.go:14,65` | comment, and the admission error: "started on a default queue before its tools required coding execution" |
| `ops/local/compose.coding-worker.yaml:2` | comment |
| `docs/interfaces.md:689`, `docs/native-cutover.md:69,84,120,129` | wording |
| `docs/2026-09-12-minimal-runtime-plan.md`, `docs/2026-09-12-helpin-native-live-evaluation.md` | wording; historical docs, edit only the image and worker terms |

Leave untouched: `codingSupported` ldflag, all references to the support agent, support product, or support workflows, and the Helpin repository.

**Verification.**

- `go build ./...` and `go vet ./...`.
- `go test ./cmd/agent-runtime-worker ./internal/engine ./internal/durable`, plus any test asserting the two changed error strings.
- `docker build -t agent-runtime:local .` and `bash scripts/container-default-smoke.sh agent-runtime:local`, confirming the default target still resolves and the smoke still rejects `--coding`.
- `grep -rn "support image\|support worker\|support queue\|container-support-smoke\|AS support"` returns nothing outside the support product's own code.

**Sequencing.** Follow-up commit on the feature branch after the edit-tool and race fixes, before the PR is opened, so reviewers see the final vocabulary.
