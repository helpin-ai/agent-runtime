# Helpin dev native harness evaluation — 2026-09-12

Tested the deployed Helpin frontend and authenticated application API with the
user-provided test account. Forge and Lens already resolve to `native_sdk` with
OpenAI `gpt-5.6-terra` in the deployed preset API and effective agent records.
No runtime-setting edit was necessary.

## Deployment failures found and fixed

1. The host API and default worker were running without a coding worker. Added
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


## Follow-up implementation and live verification

The three actionable gaps above are now addressed:

- `run_command.working_directory` is available in the runtime and Helpin tool
  catalog. Paths must be existing directories within the workspace without
  symlinks; malformed/unknown arguments fail instead of silently using the root.
- Helpin exposes **Preview changes** on task Delivery and manual run forms.
  The setting is saved on the run, inherited by continuations/children, displayed
  on the transcript, and enforced by repository specs, runtime finalization,
  publishing tools, and both Helpin PR-finalizer layers. Preview checkouts remain
  local under manual cleanup. Automatic review handoff is hidden for previews
  because another run has a separate checkout; local snapshot transfer is deferred.
- Removed the remaining custom-agent capability-to-Codex selector. Migrated four
  saved Usermaven custom agents through the normal update API, including their
  active versions. The Competitive Intelligence Digest record also exposed an
  authorization helper that validated obsolete skills before allowing repair;
  authorization now checks workspace/team access independently, and the update
  and launch paths still validate execution configuration.

Live task **USE-498** (disposable fixture only):
[Forge preview f7932f29](https://helpin-dev-fe.tryunhide.com/w/usermaven/automation/activity?run_id=f7932f29-94c1-433d-9ea3-7da2846f432a).
The UI-created initial preview run `975fddb0-47f9-4614-8dd4-f1f9067c9593`
overlapped the dev API reload and failed at target-context lookup before any model
work. Continuing it after API readiness created the successful run above and
preserved `delivery_mode=preview`.

The successful run executed `python3 -m unittest -v` in
`.native-preview-smoke-20260912`, reproduced two failures among three assertions,
used `edit_file` to replace subtraction with addition, then passed all three.
Independent post-run checks confirmed:

- `repository.pushed=false` and `delivery_mode=preview` in the completed summary;
- checkpoint version 60 with 35 messages;
- retained checkout with the fixture untracked and zero commits ahead of main;
- independent rerun of all three tests passed;
- GitHub returned 404 for `native-smoke/20260912-preview` and zero pull requests
  for that head, including closed PRs.

The preview fixture is deliberately retained for inspection. Operators must
remove retained preview checkouts when no longer needed. This adds neither
host-loss recovery nor an OS/network sandbox. Preview controls repository
publishing; other authorized host tools remain capable of their usual effects.


Follow-up validation: runtime tool/workspace suites and vet passed; Helpin
service/handler/contract suites, `go vet ./...`, and `go build ./...` passed.
The affected frontend suites passed (204 tests), followed by the new preview
pipeline regression (14 tests in that suite). Type checking and the production
frontend build passed. All four dev readiness endpoints returned HTTP 200.
Changes are committed locally in the runtime and Helpin repositories; deployment
verification above is for development, not a production rollout.
