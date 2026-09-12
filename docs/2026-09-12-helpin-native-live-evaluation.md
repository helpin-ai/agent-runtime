# Helpin dev native harness evaluation — 2026-09-12

Tested the deployed Helpin frontend and authenticated application API with the
user-provided test account. Forge and Lens already resolve to `native_sdk` with
OpenAI `gpt-5.6-terra` in the deployed preset API and effective agent records.
No runtime-setting edit was necessary.

## Deployment failures found and fixed

1. The host API and support worker were running without a coding worker. Added
   the opt-in Linux development Compose configuration and started a separate
   coding container polling `helpin-agent-native-coding`. It shares the existing
   SQLite state and Temporal namespace, retains checkouts in a named volume, and
   exposes readiness on loopback port 8092. Support queues remain separate.
2. Temporal's SDK panics when workflow task concurrency is 1. The coding worker
   now has two workflow slots while activity execution remains serialized at 1.
   A regression test constructs workers using the actual SDK for every queue.
3. The release image's CGO-disabled binary cannot open this dev SQLite store.
   The local Compose configuration mounts a CGO-enabled development worker
   binary into the coding toolchain image. Production's Postgres image does not
   need this override. See [setup instructions](native-cutover.md#linux-host-development).
4. Repository finalization ignored the outcome and committed/pushed changes
   when a review paused. It now delivers only on successful completion. Tests
   verify that pauses, failures, and cancellation preserve staged work, HEAD,
   and remote refs. Completed no-op runs also leave a new remote branch absent.
5. Forge's unconditional commit instruction induced an empty commit after a
   disposable fixture had been removed. Updated both runtime and Helpin skill
   copies to commit only actual requested file changes.

The worker fixes are running in the dev coding container, and the Helpin dev API
was reloaded with the updated no-change instruction. The example is for
trusted development on this Linux host, not a production tenancy boundary.

## Live evidence

| Check | Evidence | Result |
| --- | --- | --- |
| UI launch without repository | Helpin task HEL-2 | Task creation succeeded; run rejected with `task has no delivery target configured`. The create form displays the team repository as read-only. |
| Forge patch and verification | [Run 07319d50](https://helpin-dev-fe.tryunhide.com/w/usermaven/automation/activity?run_id=07319d50-93c4-4554-99ec-3fa8cb529433), task USE-496 | Real checkout; two failing assertions out of three; `edit_file` corrected subtraction to addition; all three passed; fixture removed; clean worktree. Exposed the empty-commit/delivery issue above. |
| Lens finding and decision | [Run 7ad28096](https://helpin-dev-fe.tryunhide.com/w/usermaven/automation/activity?run_id=7ad28096-8482-4c42-a4e1-b796fc34a3c8), task USE-497 | Identified missing multiplication by 100, cited `calc.py:2`, and paused with structured findings visible in Helpin. UI `Request changes` resumed implementation; both tests passed. Exposed publishing on pause. |
| Lens repeat after fix, container recreation, and resume | [Run 6d850617](https://helpin-dev-fe.tryunhide.com/w/usermaven/automation/activity?run_id=6d850617-ca62-4cbb-a803-6f3ae4f82f43), task USE-497 | Paused with uncommitted fixture and zero commits ahead. Checkpoint version 38 held 21 messages with `managed=false`. Container recreation preserved checkpoint, HEAD, and files. UI follow-up resumed the same run, reproduced failure before editing, passed both tests, removed fixture, and completed with `repository.changed=false`. HEAD remained identical to `origin/main`. |
| Forge no-change completion after instruction/delivery fixes | [Run e90f6572](https://helpin-dev-fe.tryunhide.com/w/usermaven/automation/activity?run_id=e90f6572-f1f3-4b9a-9f68-7da135d7bed3), task USE-496 | Executed the two requested Git checks, confirmed a clean worktree and zero commits ahead, and completed with `repository.changed=false`. No empty commit or remote branch was created. |

Tests used disposable fixtures in a checkout of `usermaven/usermaven-website`.
The first two runs unexpectedly produced PRs 215 and 216 through backend
delivery. Both PRs were closed and their test branches removed after verifying
exact HEADs and confirming that their diffs contained only the empty commit or
temporary fixture files. Product branches were not merged or modified.

## Remaining small gaps and limits

- `run_command` has no working-directory field. Forge attempted disallowed
  `env -C` and `test` commands before using Python workarounds. A validated
  repository-relative working directory would remove avoidable tool failures.
- Successful runs still use the configured backend delivery policy. A prompt
  saying “do not push” is not an API-enforced preview/no-publish mode, and the
  model's final message can omit backend delivery actions. Expose an explicit
  delivery mode if preview-only coding is a product requirement.
- The test account also has a saved custom agent referencing a retired runtime
  in its skill configuration. Built-in preset verification does not replace the
  inventory and migration of custom configurations.
- These are bounded functional checks. They establish patching, review,
  decisions, and restart/resume behavior; they do not establish general coding
  quality, host-loss recovery, or production cutover completion. The earlier
  model fixture suite covered multi-file edits; this live pass did not add a
  multi-file product change.

Validation: affected runtime worker/workspace/engine/skills tests, actual Temporal
worker construction, Helpin prompt/contract/skill tests, Helpin `go vet ./...`
and `go build ./...`, runtime vet for changed packages, and Compose validation.
