# Production coding-run latency and tool failures

Investigation date: 2026-09-26. Scope: recent Forge and Lens runs in the
production `agent-runtime` and `helpin` namespaces. The initial diagnosis was
read-only; the production workspace rollout was completed later on the same
date and is recorded below. No application code was changed.

## Evidence and measurement limits

The investigation used run IDs, timestamps, event types, tool names, durations,
token counts, nonsecret model settings, and fixed-pattern error counts from the
two production PostgreSQL databases. It also inspected the coding-worker image
(`v0.13.2`), startup command, workspace filesystem, and source code. No secret
values, prompts, tool arguments, or raw tool output were read.

The later loop investigation used server-side hashes, lengths, equality checks,
and fixed-phrase counts on tool metadata. No full prompt, file content, tool
argument, or tool output was selected. Production was kept read-only.

Broad production worker-log retrieval was rejected by automatic approval review
because logs could contain prompts or credentials. A query for full
`native_context` JSON was rejected for the same reason; scalar model settings
were queried instead. Consequently, exact Go and pnpm error text is not
established here. The runtime also lacks separate queue, repository-spec,
clone, checkout, and sync timing spans, so their shares of preparation time
cannot yet be measured directly.

The four most recent relevant runtime runs on 2026-09-25 (UTC) were:

| Agent | Runtime run ID | Preparation gap | Model rounds / estimated time | `run_command` calls / summed time | Outcome |
| --- | --- | ---: | ---: | ---: | --- |
| Forge | `run_b79002f65d0cf8a0edc19dd6` | 256 s | 13 / 110 s | 33 / 193 s | Completed |
| Lens | `run_fcc3d552415d5a9e1020e6d8` | 139 s | 24 / 194 s | 44 / 197 s | Completed after a human pause |
| Forge | `run_312a851b4ab340a2f4afbe91` | 101 s | 82 / 640 s | 42 / 438 s | Completed |
| Lens | `run_be014edfcd8b86cd3bc65bd7` | 125 s | 98 / 702 s | 31 / 288 s | Cancelled after a human pause and resume |

Preparation is from `run.queued` to the first `workspace.prepared` event. Model
time is estimated from each `context.usage` event to its corresponding assistant
completion. Command time sums `tool_call_finished.duration_ms`; overlapping
calls can make this sum larger than elapsed wall time. Human pause time must be
excluded from execution comparisons.

## 1. Repository preparation takes minutes

**Finding.** Forge and Lens require a repository workspace. A new run creates
its own checkout, performs a full `git clone`, checks out and synchronizes the
branch, then removes the run tree at terminal cleanup. The clone uses neither
`--depth` nor a partial-clone filter. See
[repository preparation](../internal/workspace/git_provider.go) and the Helpin
repository-workspace selector in
`helpin/server/internal/service/agent_runtime_launch_context.go`.

Before the rollout, production started the coding worker with `--coding`, without
`--ephemeral-workspaces`. Its checkout is on the `juicefs-r2` PVC, mounted as
`fuse.juicefs`. The worker's separate private scratch is on local ext4. A local
Helpin checkout has about 676 MB of Git data; its size is contextual evidence,
not a measurement of the production clone.

Follow-up on 2026-09-26: `gitops/agent-runtime/stage/kustomization.yaml` includes
both `ephemeral-workspaces` and `node-local-repository-cache`. The stage render
uses `emptyDir` for the checkout and keeps the old JuiceFS PVC object only to
avoid pruning retained data. At diagnosis,
`gitops/agent-runtime/prod/kustomization.yaml` included only `coding-worker`;
its render and the live production Deployment mounted the JuiceFS PVC.
Production has two Landlock-labelled coding nodes,
but the second node's local ext4 filesystem was already 82% used at inspection.
The runtime database contained 210 paused repository runs with retained
workspace leases (189 Ask Agent, 19 Scribe, 2 Lens). Eighteen have
`native_coding=true` and are definitely routed to the coding queue. The other
192 have `false` or no flag and were created under older routing; their actual
historical Temporal queues have not been checked. An old non-session workflow
on the coding queue cannot resume on an ephemeral worker; see the fail-closed
guard in `internal/durable/activities.go`. At the follow-up check there were no
queued or running runtime runs. The chosen simple retirement path is to cancel
all 210 paused repository runs through the runtime cancellation API before
switching the coding queue. This retires old checkouts without maintaining a
separate legacy-worker pool. Helpin's `ContinueTerminalRun` can generally
start a new child run from a cancelled run with the same agent, target, and
branches, provided those still exist. It cannot resume the exact execution or
restore unpublished files, local-only commits, or pending approval state. Keep
the old PVC object during rollout; deleting the volume is a separate decision.

The production GitOps change adds the same two components as stage,
keeps the old PVC rendered, and pins coding pods to the two verified production
Landlock nodes. A Kustomize render confirms that the API receives
`AGENT_RUNTIME_WORKSPACE_STORAGE=ephemeral`, while the coding worker receives
that setting plus repository-cache mode, starts with `--ephemeral-workspaces`,
and mounts an `emptyDir` rather than the PVC. The deployed `v0.13.2` worker
binary advertises the required flag. It was published as GitOps commit
`5fe7fd13f9e851f10acd522c373503efa5b285d0` and synced to production on
2026-09-26.

## Proposed production cutover sequence (saved 2026-09-26)

The staging Ask Agent failures were diagnosed on 2026-09-26: both shared and
coding workers reached their first Anthropic `claude-sonnet-5` request, which
returned HTTP 400 `invalid_request_error` because the account credit balance
was too low. The user supplied the exact provider error. All five staging runs
on that model failed before a tool call; unauthenticated HTTPS reached Anthropic
from both worker pools. TypeSafe's Jev command reviewer runs only after a tool
call and falls back to the existing approval policy if unavailable, so its key
did not cause these run failures. This is a model-funding gate, not evidence of
an ephemeral-workspace failure. A fresh coding run on a funded model is still
needed for independent end-to-end staging verification; the user subsequently
reported that staging was working. The local staging workspace benchmark and
recovery tests are recorded in `ephemeral-workspaces.md`.

The sequence is:

1. Announce a short coding-run maintenance window and temporarily disable
   automatic sync for the `argocd/agent-runtime` Application if enabled. It
   points at `agent-runtime/prod`; publishing the manifest before the drain
   with auto-sync enabled would otherwise start the cutover immediately.
   Auto-sync was already off when the actual drain began.
   Recount queued, running, and paused repository runs immediately before
   acting. Confirm every run selected for cancellation is still paused and
   mapped to a Helpin run. Do not cancel any run that became active without
   reviewing it.
2. With the old workers still running, cancel the paused repository runs through
   the runtime API, **not** by changing database status directly. That API
   cancels the Temporal workflow, attempts checkout cleanup, clears run
   credentials, and emits the host cancellation event. Verify corresponding
   Helpin runs become cancelled. Stop if any cancellation or projection fails.
3. Scale the old coding worker to zero, which removes coding queue pollers and
   makes new coding admission fail closed. Recount to catch any run started
   during the drain; resolve it before proceeding.
4. Publish and manually sync the prepared manifests while the coding queue is
   stopped. Argo sync waves place the API at `-1` and the coding worker at `1`,
   so the API acquires the ephemeral setting before the five new workers poll
   the queue. Restore automatic sync only after both deployments are healthy.
   Keep the old PVC object and data intact.
5. Verify a new Forge run prepares on `emptyDir`, reuses the node-local cache,
   pauses/resumes, and completes delivery. Check node free disk, pod evictions,
   session capacity, PostgreSQL connections, and queue pollers. The second
   production node had 632 GB free (82% used) at inspection.

Readiness checks immediately before step 1: verify a funded model route in
production; render the production overlay and confirm matching `v0.13.2`
API/coding images, local checkout and cache mounts, node affinity, and Argo
sync waves; confirm the two target nodes still have enough local disk; and
record the current Argo Application status and run counts. Treat a failed
staging Anthropic run as the known credit issue until a funded-model test can
exercise the workspace path.

Read-only readiness snapshot on 2026-09-26: the production Application is
Synced/Healthy with auto-sync self-heal enabled. The production render passes;
both images resolve to `v0.13.2`, the API has ephemeral mode, the coding worker
uses `--ephemeral-workspaces`, a 20 GiB `emptyDir`, a node-local cache mount,
and a 44 GiB pod ephemeral-storage limit. Both pinned ax162 nodes are Ready
with Landlock `abi3`; their worker-local filesystem had about 4.1 TB and
603 GB free, respectively. Recent production runs completed with OpenRouter
and ChatGPT routes; this does not verify production Anthropic credit. The
run count must be refreshed immediately before any drain.

Rollout record on 2026-09-26: all 210 paused repository runs were cancelled
through the runtime API in one canary and five checked batches. The canary
projected as cancelled in Helpin; all 209 batched host run IDs projected as
cancelled, with zero paused repository runs and zero queued/running runtime
runs after the drain. The old coding Deployment was scaled to zero before
publishing the GitOps commit. A Kubernetes server-side dry run accepted the
rendered resources. Manual Argo sync completed at the commit above; the API
was Ready 2/2 and coding workers Ready 5/5, with the Application Synced and
Healthy. Both production coding nodes showed the checkout and cache mounted
on local ext4, cache directory mode `0700` owned by UID/GID 1000, and the
intended storage/cache environment settings. Auto-sync self-heal was restored.
The first observed new production coding run, `run_c365b2f31567c3ec9b360b7c`,
was queued at 06:28:07.254 UTC and emitted `workspace.prepared` at
06:28:36.370 UTC: a 29.116-second preparation gap. Its lease root is under
the session-scoped local workspace path, and it has an execution-session
marker. It reached model and tool execution (11 model usage events and 15
tool-call records at inspection) and was still running, so successful terminal
delivery and pause/resume have not yet been verified. This is one post-rollout
sample, not a new latency distribution. The old JuiceFS PVC remains rendered
and unused by the coding worker;
the `workspace.cleaned` event alone does not prove every old checkout was
physically removed from that volume.

The delay is recurring and repository-dependent. For runs started during the
preceding seven days, median Helpin-repository startup gaps were 109 seconds for
Forge, 125 for Lens, and 165 for Scribe. The smaller `contentpen` repository's
seven runs had an 8-second median. The same source tree's
[staging benchmark](ephemeral-workspaces.md#verification-2026-09-22) measured a
fresh-checkout install/build/test workload at 648.85 seconds on JuiceFS versus
67.85 seconds on local `emptyDir`, both with warm dependency downloads. That
benchmark did **not** measure Git clone time.

**Root-cause confidence.** A cold full clone into JuiceFS is the strongest
explanation, supported by the code, deployed filesystem, repository-size
pattern, and staging filesystem comparison. The precise contribution of queue
wait and each preparation substep remains unknown until instrumented.

**Work to tackle:**

- [ ] Add safe duration metrics for queue wait, repository-spec lookup, clone,
  checkout, branch sync, and cleanup, tagged by repository identity and worker
  pool without logging URLs with credentials or command output.
- [x] Roll out the already implemented local ephemeral coding workspace and
  node-local repository cache after draining active and retained runs. Follow
  the [rollout and recovery guide](ephemeral-workspaces.md#rollout). Worker loss
  then reclones; unpushed edits and local-only commits are lost, so verify
  recovery behavior before production rollout.
- [ ] After measuring local-disk preparation, evaluate a safe partial/shallow
  clone or node-local Git object cache. Keep branch sync, merge-base checks,
  authorization, and PR delivery correct before enabling it.
- [ ] Verify p50/p95 time from queue to first model request and time spent in
  terminal checkout cleanup on the same repository cohort.

## 2. Execution is slower than a comparable Codex session

**Finding.** Per-round model latency was approximately 7–9 seconds, but two
long runs made 82 and 98 rounds. They consumed 3.14 million and 4.41 million
cumulative input tokens, of which 2.50 million and 3.71 million were reported
cached. Estimated input reached 83k and 91k tokens, respectively. Context
generation remained zero throughout these runs, so no compaction was observed.
The runtime resends the growing transcript to the model on each step; see the
[native execution loop](../internal/runtime/native_exec.go).

All four runs used `gpt-5.6-terra` with `high` reasoning effort, the standard
service tier, and a 2,000-step ceiling. The service-tier mapping in
[native_model_controls.go](../internal/runtime/native_model_controls.go) sends
`standard` as the provider's default tier. The high ceiling did not itself
terminate these runs. [Official OpenAI reasoning guidance](https://developers.openai.com/api/docs/guides/reasoning)
describes `high` as a quality-over-latency choice and recommends evaluating
`medium` for suitable agentic work.

Commands also consumed hundreds of seconds. The longest Go commands took 240,
212, 180, and 133 seconds; a Python command took 180 seconds. Several long Go
calls ended with errors, including compile/test signatures. The production
repository-cache mode is unset, so [tool caches](../internal/workspace/tool_cache.go)
are private to the run and cannot warm later runs of the same repository.
Completed runs spent another roughly 34–40 seconds between workspace
finalization and completion, consistent with checkout cleanup on the shared
filesystem.

**Root-cause confidence.** The time split is measured: many model rounds plus
long commands account for most active execution. Shared-filesystem I/O and cold
per-run caches have strong support from deployment and staging measurements.
The investigation did not capture a paired Codex trace with the same task,
repository commit, prompt, tool permissions, reasoning effort, and service
tier. The model name alone does not make harness timings equivalent; see
[official OpenAI agent-runtime distinctions](https://developers.openai.com/api/docs/guides/agents).

**Work to tackle:**

- [ ] Move checkouts, dependency trees, and build outputs to local disk and
  enable the node-local cache as part of issue 1. Re-measure Go, pnpm, and
  cleanup duration by repository.
- [ ] Run paired task evaluations with matched model, effective controls,
  repository commit, and acceptance tests. Compare verified outcomes, active
  duration, model/tool rounds, input/cache/output tokens, and cost. Exclude
  human pause time.
- [ ] Evaluate `medium` against `high` reasoning effort and an optional faster
  service tier only with a quality and cost gate. Improve tool guidance and
  batching to reduce avoidable round trips. Evaluate compaction carefully:
  changing a prompt prefix can reduce cache reuse.
- [ ] Establish p50/p95 targets for preparation, active run time, model time,
  command time, and successful verified patches before changing defaults.

## 3. Tool calls fail frequently

**Reassessment (26 September, after the local-workspace rollout).** The
`error` counter is a poor proxy for tool reliability. It counts a launched
command's nonzero exit as a tool error, including tests that detect a code
defect, a missing file, and an `rg` no-match result. The first new production
coding run had 15 completed tool calls (six commands, eight file reads, one
repository search) and zero recorded errors when checked. That is a small
sample, not a reliability claim. The four older runs still contain a concrete
configuration mismatch: seven `sed` calls were denied by the runtime
allowlist. The repository-search timeout and missing-path calls are worth
investigating if they recur. Do not treat the aggregate of 59 errors as 59
platform failures or change command exit semantics only to lower that number.

**Decision.** No urgent production rollout is warranted for #3 on this
evidence. If failures recur, first separate command exits, policy denials,
invalid paths, and infrastructure errors in telemetry; then target the category
that affects completed work. A small tool-description improvement can steer
line-range reads to `read_files` rather than unavailable `sed`.

**Finding.** Across the four runs, 59 of 338 completed tool calls recorded an
error. `run_command` accounted for 52 errors in 150 calls. Breakdown from
nonsecret tool names and fixed error signatures:

| Program or tool | Calls / errors | Established pattern |
| --- | ---: | --- |
| `rg` through `run_command` | 48 / 22 | 15 errors contain a missing-file signature; most failed calls ran from the repository root. Four contain exit status 1, which can mean no match. |
| `sed` through `run_command` | 7 / 7 | Rejected as disallowed. `sed` is absent from the [runtime command allowlist](../internal/tools/workspace_command_tools.go) and the Forge/Lens profile lists in `helpin/server/internal/agentcontract/runtime_profiles.go`. |
| `go` through `run_command` | 15 / 10 | Several compile/test failure signatures; some calls ran for minutes. |
| `pnpm` through `run_command` | 3 / 3 | Package-manager or missing-file signatures; exact cause unverified. |
| `repository_search` | 48 / 3 | One missing path, one search error, one approximately 30-second timeout. |
| `apply_patch`, `edit_file`, `read_files` | 125 / 4 | A small number of edit/read errors; exact text unverified. |

The remaining `run_command` errors are from `cat`, `python3`, `head`, `ls`,
`mv`, and `rm`. The [command implementation](../internal/tools/workspace_command_tools.go)
returns a tool error for every nonzero process exit, including an expected
search miss or a test that correctly found a code defect. It also accepts
structured `program` plus `args` and does not process shell operators in the
`command` shorthand. The recorded aggregate therefore overstates tool-system
failures; it still represents extra model recovery work and a poor user
experience.

**Work to tackle:**

- [ ] Return a structured result with exit code, stdout, stderr, and timeout
  status for commands that actually launched. Reserve tool errors for invalid
  input, policy denial, spawn failure, and infrastructure faults. Keep output
  bounds and secret-safe telemetry.
- [ ] Align the advertised command list and tool descriptions with the actual
  runtime allowlist. Either support `sed` inside the existing sandbox or steer
  file viewing to `read_files`; verify Forge and Lens use the advertised path.
- [ ] Have repository search distinguish no matches, invalid patterns, missing
  paths, and timeout. Return actionable path hints after a missing path, and
  encourage directory discovery before path-specific searches.
- [ ] Track policy rejections, input errors, normal nonzero exits, test/build
  failures, package failures, timeouts, and infrastructure failures separately.
  Use that taxonomy to re-evaluate the 59-error baseline after fixes.

## 4. Lens loops and cumulative token use (follow-up at 12:20 UTC)

**Finding.** The clearest runaway run was Lens
`run_deb2ad79bd8c884ec63fdb00`, started at 10:21:58 UTC and cancelled at
11:33:45 UTC. From 11:07:53 to 11:33:41 it made 207 `edit_file` calls against
one path; all 207 had `old_string == new_string`, and 204 had identical complete
inputs. They came from 207 distinct assistant messages, not one parallel batch.
The runtime recorded zero edit errors, and a server-side fixed-phrase check
confirmed that every edit result said `No changes made`. Reconstructing the
provider's streamed tool arguments inside PostgreSQL showed 207 valid raw
calls with identical old and new text; the persisted inputs matched those raw
calls. The model emitted these no-op requests; tool-call assembly did not
invent them. The run required cancellation to stop.

The no-op pattern also occurred in other recent runs: 20 of 30 edits in one
Lens run, 17 of 19 in another, and 10 of 11 in one Forge run. This is not
unique to one file or one run. The deployed `v0.13.2` `edit_file` implementation
intentionally returns a successful result for unchanged content; its test
asserts `No changes made`. The normal model replay path sends that result back
to the model. The runtime has no progress-based repeat guard; it checks only
the configured maximum tool steps and optional cumulative token budget.

The visible repeated file reads are a different pattern. In the runaway run,
the most-read path was requested 12 times in 12 distinct line windows; only
two `read_files` calls happened after the no-op edit loop began. Across recent
Lens runs, an identical file-and-range request occurred at most twice (five
such duplicates total); Forge had none. A filename-only view can make normal
line-range paging look like repeated reads. The pathological loop here was
no-op editing.

The run accumulated 13,884,001 input tokens, including 13,091,584 reported
cached tokens. Approximately 13,096,820 input tokens accrued during the
26-minute no-op edit period. Estimated input per request grew from 18,720 to
121,158 tokens; context generation stayed zero, so no compaction occurred.
The affected built-in Lens agent uses the `review_agent_interactive_loop`
preset, has no stored custom system prompt, and is configured for 2,000 tool
steps with no `native_context.max_total_tokens` budget. Helpin commit
`2d3f4bf55` changed the default workflow maximum from 50 (300 for selected
agents) to 2,000 for all agents; the launch path supplies that value to the
runtime. The large ceiling allowed the repeated no-progress calls to continue
until cancellation. The model's internal reason for repeating the edit cannot
be established from metadata alone; the raw tool-call pattern and runtime
response are established.

**Recommended fix order, without a production change during this inquiry:**

1. Reject `edit_file` calls with identical old and new text as explicit no-op
   input errors, and return structured `changed=false` for other edits that
   leave bytes unchanged. Keep file-read preconditions and unique-match checks.
2. Add a checkpointed, run-local progress guard. Fingerprint exact tool name,
   normalized input, and relevant workspace state. After a small number of
   identical no-progress calls, tell the model why the action is blocked; if
   it persists, pause or terminate with a `loop_detected` reason. Distinguish
   sequential line windows from identical read requests.
3. Set a practical Lens-specific step and cumulative-token budget, with usage
   alerts and a safe terminal outcome. Enable bounded context management to
   limit replay size, but do not rely on compaction to stop a behavioral loop.
4. Add one concise instruction to the Lens/Forge coding guidance not to retry
   an edit that reported no change; treat the prompt as a supporting measure,
   not the only guard. Show read ranges and no-change edit status in the run UI.
5. Verify with a fake model that repeats the same no-op edit, plus a legitimate
   multi-window file read and a legitimate repeated edit after a file change.

## Suggested order

1. Add phase and failure-category metrics so each later change can be measured.
2. Roll out local coding workspaces and node-local caches with drain and recovery
   verification.
3. Correct command result semantics and Forge/Lens tool guidance.
4. Optimize clone behavior and model-round count after the new timing baseline.
5. Run the matched Codex comparison and keep changes that improve verified
   outcomes and latency together.

## 5. Effective provider, epic profile, and replay (follow-up)

The stored runtime Agent rows for Lens and Forge retain an older provider/model
default. That default did **not** determine the sampled runs. Their run inputs
all specify `openai_chatgpt` and `gpt-6-sol`; `resolveProviderAndModel` gives the
run input precedence over the Agent row. The four sampled production Lens and
Forge runs all belong to epic plan
`602466d8-f163-4411-9794-1450ab644353` and record the same AI profile ID,
`8188caf6-2cbb-40b1-b4fa-aabd01699fd3`, which matches the plan's saved
profile binding. Across that plan, all 21 AI runs since September 25 use that
ChatGPT profile and `gpt-6-sol`. One older Forge run from September 22 used a
different OpenRouter profile before the plan profile binding change. No
credentials or prompts were read for this verification.

The deployed Helpin `main` path resolves an explicitly selected epic profile
once, saves the selection privately on the plan, and reloads it for each DAG
step. The observed child runs confirm that propagation for this plan. An
explicit paused-step restart can replace the plan's profile binding for the
restarted and later steps; it does not rewrite already completed runs.

The `openai_chatgpt` transport sets `store=false` and deletes
`previous_response_id`. Each model turn is built from the saved native message
history and sent as a fresh request. Provider-side cached input can lower the
effective cost, but the context still grows; this run's estimated input reached
121,158 tokens per request. Bounded compaction is useful for legitimate long
runs and should be configured against the model's input limit after the loop
guard. Compaction alone would have summarized a repeated no-op loop, not stopped
it.

A local-only branch `fix/native-no-progress-loop` in
`/private/tmp/agent-runtime-no-progress` rejects unchanged `edit_file` calls
and checkpoints a guard that injects one runtime recovery notice after three
consecutive identical no-progress tool results. The model can continue with a
different action; five consecutive identical results stop the run if recovery
fails. The fix has not been pushed or deployed.
