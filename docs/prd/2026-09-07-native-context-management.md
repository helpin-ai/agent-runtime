# Native SDK context management: harness findings and implementation plan

Status: initial portable implementation and regression tests added; opt-in, not
deployed or enabled on presets. Research/implementation date: 2026-09-07.

## Implemented scope and safe rollout

The current patch implements the first portable path, not every optimization in
the plan below:

- Separate `native_run_states` and append-only `native_run_journal` tables, created
  by runtime `AutoMigrate`. Checkpoint replacement and journal appends use one
  transaction with app/run-scoped version checks. Raw retired history is retained;
  only the active model replay is replaced. No existing rows are rewritten.
- Per-response cumulative usage, including observed partial/error responses and
  summary calls; bounded detached accounting writes on cancellation. Usage writes
  do not update the shared run row or overwrite host cancellation. The engine
  recovers checkpoints on failures and corrective turns and emits cumulative
  checkpoints. Existing delta-style custom native adapters remain compatible.
- Helpin normalizes native/Codex inclusive completion counts into ordinary output
  plus reasoning; OpenCode's exclusive output convention remains separate. No
  historical charge adjustments are made by this patch.
- Opt-in pre-request compaction: historical summary plus recent complete
  call/result groups, latest real user request, and loaded skill groups. System
  instructions and tool definitions are rebuilt independently. Summaries expose
  no tools and require complete response metadata. Failed/truncated/non-shrinking
  summaries preserve history, use a soft-threshold cooldown, and fail closed at
  the hard input limit. Context-overflow recovery has one retry per failed request.
- Requests reserve configured output headroom, check cancellation, and optionally
  enforce a cumulative per-run token allowance. Resumes inherit that allowance.
- `read_files` retains its cursor envelope; content increases from a shared 2,100
  to 8,192 characters only with managed native context enabled. Other runtimes'
  read budgets are unchanged. Generic truncation guidance no longer encourages
  repeating mutating actions to recover output.

### Activation

Deploy the additive runtime migration and the Helpin config/accounting changes
before enabling a canary. Enable on a dedicated/custom native agent first;
system-managed preset reconciliation can restore preset-owned configuration.
No shared SDK schema change is needed: the policy travels in existing execution
config JSON, and usage/compaction events use existing generic event payloads.

Example `execution_config` fragment (illustrative limits, **not** model metadata):

```json
{
  "native_context": {
    "enabled": true,
    "context_window": 128000,
    "max_output_tokens": 16000,
    "trigger_tokens": 48000,
    "keep_recent_tokens": 12000,
    "summary_tokens": 4000,
    "safety_tokens": 4000,
    "max_total_tokens": 2000000
  }
}
```

Use the actual resolved provider/model's limits; supply `input_limit` if it has a
separate smaller input cap. Enabling without an explicit valid context window is
rejected. The default trigger is min(64,000, input limit), not a guarantee that an
oversized protected user/skill message fits. `max_total_tokens` is optional and
also works with `enabled: false`; retain it when rolling back auto-compaction.
Custom model factories must implement `NativeSummaryModel` when enabling this.

Observe `context.usage`, `context.compaction_started`,
`context.compaction_completed`, `context.compaction_failed`, and cumulative
`usage.checkpoint` events. Compare baseline/canary task correctness, input/cached
input totals, summary overhead, repeated read ranges, failures and latency.
Disable the policy if correctness or resume/approval behavior regresses. Keep
the new checkpoint-capable runtime deployed: toggling the policy off retains the
bounded active checkpoint, whereas rolling back to an older binary mid-run does
not have equivalent recovery guarantees.

### Safety boundaries and remaining work

- An interrupted managed execution checkpointed in the tool phase is deliberately
  not replayed automatically: tool side effects might have happened before their
  durable result. It stops with a review-required error. Normal approval pauses
  and resumes are supported and regression-tested. Recovering ambiguous tool
  execution automatically requires tool idempotency/outcome reconciliation.
- Version checks prevent stale commits, not concurrent provider calls by duplicate
  workers. Provider usage that is never returned (e.g. a hard worker crash), or
  cannot be committed during a storage outage/conflict, still needs reconciliation
  against provider records. Do not interpret this as exactly-once billing.
- Token sizing uses a conservative estimate of the bounded replay plus system
  instructions/tool schemas, calibrated by observed input usage; it is not an
  exact provider tokenizer. Model limits are operator-supplied for this first
  version. The allowance is not an exact monetary or cross-subagent spend cap.
- Extremely large protected messages/tool arguments can still fail safely rather
  than fit. Larger reads do not solve arbitrarily long single-line files. General
  saved-output retrieval, old-output pruning, provider-native compaction, dynamic
  model-limit discovery, journal retention/inspection UI, and shared delegation
  budgets remain follow-ups.
- Automated tests cover 120 tool rounds with repeated compaction, grouped replay,
  skill preservation, resume/rollback, approval execution once, summary failure,
  budgets, cancellation, storage CAS/transaction rollback, accounting, cursor
  integrity, and real provider serializers against local HTTP servers. These are
  not live model-quality or production Postgres evaluations. Run controlled
  documentation/code-investigation canaries before broader activation.

## Recommendation

Use Pi's portable summary-plus-recent-history design as the initial foundation,
Codex's context lifecycle and accounting separation as the reliability model,
and OpenCode's older-output pruning as a later optimization. Make tool reads
large enough to be useful, with exact continuation or saved-result retrieval.
Implement provider-native compaction only as a capability-specific second path.

Context compaction limits the size of each request. It does not cap cumulative
spend or guarantee that an agent stops researching. We need three separate
controls: per-request context limits, cumulative usage limits, and progress/step
limits. Increasing context windows or reducing tool-output limits alone does not
solve the repeated-input problem.

## Sources and scope

Fresh public checkouts are in `/tmp/harness-context-study.QklvdS/`. The existing
`/tmp/codex` contained directories but no usable source files or Git HEAD.

| Repository | Inspected revision | Relevant sources |
| --- | --- | --- |
| Codex | `7769bccbb2b4e9469a36b12510e73594fa03c5d5` | [turn loop](https://github.com/openai/codex/blob/7769bccbb2b4e9469a36b12510e73594fa03c5d5/codex-rs/core/src/session/turn.rs), [local compaction](https://github.com/openai/codex/blob/7769bccbb2b4e9469a36b12510e73594fa03c5d5/codex-rs/core/src/compact.rs), [remote compaction](https://github.com/openai/codex/blob/7769bccbb2b4e9469a36b12510e73594fa03c5d5/codex-rs/core/src/compact_remote.rs), [model limits](https://github.com/openai/codex/blob/7769bccbb2b4e9469a36b12510e73594fa03c5d5/codex-rs/protocol/src/openai_models.rs) |
| Pi | `e687434a60174db1a9c961d973881a7a851a0597` | [compaction implementation](https://github.com/badlogic/pi-mono/blob/e687434a60174db1a9c961d973881a7a851a0597/packages/coding-agent/src/core/compaction/compaction.ts), [session orchestration](https://github.com/badlogic/pi-mono/blob/e687434a60174db1a9c961d973881a7a851a0597/packages/coding-agent/src/core/agent-session.ts), [design documentation](https://github.com/badlogic/pi-mono/blob/e687434a60174db1a9c961d973881a7a851a0597/packages/coding-agent/docs/compaction.md) |
| OpenCode | `57ef3828431790c53f8f333c7ffbfe88770a1812` | [legacy compaction/pruning](https://github.com/anomalyco/opencode/blob/57ef3828431790c53f8f333c7ffbfe88770a1812/packages/opencode/src/session/compaction.ts), [new core compaction](https://github.com/anomalyco/opencode/blob/57ef3828431790c53f8f333c7ffbfe88770a1812/packages/core/src/session/compaction.ts), [new core history](https://github.com/anomalyco/opencode/blob/57ef3828431790c53f8f333c7ffbfe88770a1812/packages/core/src/session/history.ts), [output truncation](https://github.com/anomalyco/opencode/blob/57ef3828431790c53f8f333c7ffbfe88770a1812/packages/opencode/src/tool/truncate.ts) |

Local application baseline: Agent Runtime `87a6948`, Helpin
`c9f6c6c59178de9e90903e3a2c4e2d7a85db017a`. This is a source comparison, not a
live benchmark or attribution of the reported 48M-token run. Its run ID and
provider usage breakdown remain necessary for incident attribution.

## What the other harnesses actually do

### Codex

- Checks context before sampling and after sampling/tool execution when another
  model call is needed. Compaction can happen inside a long user turn.
- The normal model auto-compaction limit defaults to 90% of the resolved context
  window; configured limits are capped there. Additional scopes and feature
  gates exist, so this is not a universal fixed threshold for every mode.
- Selects local summarization or remote compaction according to provider
  capabilities. New remote and token-budget paths are feature-dependent.
- Replaces the active history and persists a compaction checkpoint containing
  replacement state. Recomputes active-context usage after replacement.
- Explicitly reinjects initial context around compaction. Instructions and
  permissions are not left solely to a lossy summary.
- Local summarization has an overflow fallback that removes oldest input items
  and retries. This shows why the summary request also needs a size budget, but
  silent oldest-item loss is not the default recovery we should adopt.

Take: safe loop boundaries, persisted context replacement, instruction
reinjection, model-aware thresholds, distinct active and cumulative usage.

Do not copy initially: experimental context-management modes, provider-specific
state assumptions, or context-limit percentages as our only cost policy.

### Pi

- Defaults to a 16,384-token reserve and approximately 20,000 recent tokens kept.
  The normal trigger is `contextTokens > contextWindow - reserveTokens`.
- Uses the last valid provider usage plus estimates for trailing messages. With
  no usable provider usage, estimates the messages. Zero/error usage does not
  reset the context estimate.
- Summarizes old messages with the previous summary as input, then rebuilds
  context as summary plus the retained suffix. Full session entries remain.
- Stores `firstKeptEntryId`, `tokensBefore`, summary, and summarization usage.
  The next compaction includes formerly retained messages that are now old;
  it does not accidentally skip them because a summary already exists.
- Can split a very long user turn at an assistant boundary. Tool results are not
  separated from their calls. Tracks read and modified files in summary details.
- Guards against stale pre-compaction usage triggering another compaction and
  permits one compact-and-retry overflow recovery. Rejects summaries stopped by
  an output-length cap rather than checkpointing partial summaries.
- Its read tool supports continuation offsets; shell output can refer to a saved
  full-output file. Default truncation limits are 2,000 lines / 50 KiB.

Take: this is the best initial portable algorithm for our multi-provider runtime,
particularly iterative summaries, retained boundaries, split-turn handling, and
bounded failure recovery. Adapt its token semantics; Pi's usage components are
not interchangeable with our inclusive input/completion totals.

### OpenCode

The inspected revision contains two implementations. Their behavior must not
be combined into a claim about one uniform execution path.

- Legacy session pruning protects the two most recent user turns and a 40,000-
  token tool-output region, excludes the skill tool, and only prunes when it can
  remove more than 20,000 tokens. Results are marked compacted and replaced in
  model replay; the original output is retained in stored state.
- Legacy summarization preserves a configurable recent tail, combines prior
  summary with new history, and accounts for input/output limits.
- New core estimates the rendered request, including system instructions and
  tool definitions. It triggers above context minus the larger of requested
  output and its buffer. Defaults: 20,000 buffer, 8,000 recent tokens, 4,096
  summary-output tokens. These are implementation defaults, not our targets.
- New core serializes its recent tail into the compaction record; it is not the
  same raw-message suffix approach as Pi. History loading starts from the latest
  compaction, while retaining appropriate system updates.
- The new core has a `prune` configuration field, but the inspected compaction
  implementation does not implement the legacy pruning algorithm. Do not assume
  the legacy 40k/20k behavior applies to the new core.
- The legacy tool truncator saves full output and returns a bounded preview plus
  a retrieval hint; default limits are 2,000 lines / 50 KiB.

Take: rendered-request accounting, thresholded pruning of older outputs, and
retrievable output instead of instructions to blindly repeat a command.

## Additional findings in our code

1. `internal/runtime/native_exec.go` appends all history and has no aggregate
   context budget. `native_resume.go` restores that full history on resume.
2. `native_model_visibility.go` bounds individual result strings at provider
   conversion time. It does not summarize conversation history, and it can clip
   a newly returned result before the model first sees it.
3. The underlying `read_files` is already cursor-aware: `has_more` and
   `next_start_line` exist. However, `workspace_read_tools.go` limits total content
   to 2,100 runes per call, shared across up to four files. This can amplify model
   round trips. Its JSON envelope/line numbering also compete with the separate
   2,800-rune native visibility limit. Avoid a second blind string truncation of
   already structured, bounded results.
4. Usage is accumulated in memory. The engine's checkpoint follows adapter
   completion, too late for Helpin to enforce a budget during the native loop.
   Adapter errors can discard accumulated result usage before it reaches the
   final summary. Durable incremental usage is needed even when a run fails.
5. Native assistant tool arguments can contain large document bodies. Clipping
   tool results alone does not remove those arguments from subsequent requests.
6. Helpin has model context metadata in its AI usage catalog, but the inspected
   native model request/config does not carry an explicit resolved context limit.
7. Helpin's reasoning settlement must be corrected independently: native output
   tokens are inclusive completion tokens. The current agent settlement adds
   reasoning to completion and then prices reasoning separately.

## Proposed design

### State and invariants

Maintain three distinct records:

- Append-only execution history: exact messages, original tool arguments/results,
  interactions and outcomes. Use durable sequence IDs and app/run scoping.
- Active context checkpoint: version, context generation, summary, summarized
  range, retained-message boundary, model identity, prompt/toolset fingerprint,
  and context estimates. Optional provider-owned state is a separate capability.
- Cumulative usage ledger: per-provider-request usage, including summaries,
  retries with reported usage, and partial/error usage when available. Mark
  unavailable usage explicitly; do not report invented actual tokens.

Compaction must not reset cumulative usage, run budgets, turn-completion state,
or approval state. Pending interactions and execution records remain outside the
summary. A summary never authorizes a tool call or replaces an approval record.
Call/result groups stay together. A full document-write argument can leave the
active context with its completed group once summarized; execution/audit copies
stay intact.

Persist a validated checkpoint before selecting it for the next request. Abort,
failure, or an incomplete summary preserves the previous checkpoint. Compare
sequence/version when committing so concurrent steering is not lost. Resume
loads the checkpoint and subsequent history, never the entire old transcript by
default. Old runs without a checkpoint need an explicit legacy reconstruction
path. Do not implement this by repeatedly rewriting a growing `OutputSummary`.

### Context policy

Resolve model context and input/output limits from trusted provider/model
metadata and a validated override. Budget the actual provider-rendered request:
system prompt, skills, tool schemas, summary, recent messages, tool arguments and
results. Reserve output and a safety margin before every model request.

Use provider usage as a calibration baseline plus estimated additions; invalidate
that baseline after compaction, model changes, toolset changes or prompt changes.
Track active request tokens separately from cumulative billed tokens.

Suggested initial evaluation settings, not production promises:

- Compare soft compaction targets of 64k and 96k request-input tokens, each capped
  by the model's usable input limit after output reserve and safety margin.
- Retain approximately 12k–20k recent tokens and target a 2k–4k summary, scaled
  down for small models. Account for fixed instructions/tool schemas separately.
- Require meaningful reduction below the trigger before continuing. Do not
  repeatedly summarize an unchanged prefix or accept a summary larger than the
  material it replaces. Re-estimate after replacement.
- When mandatory instructions plus the minimum working set cannot fit, return a
  specific context-capacity outcome; repeated compaction cannot fix that case.

Summarize at completed model/tool-round boundaries, including within one long
user turn. Summary content: objective, latest user requirements, constraints,
decisions, completed work, active work, unresolved questions, inspected paths and
symbols, durable document/artifact IDs, and the next action. Preserve exact
identifiers and sources; distinguish facts from assumptions. On subsequent
compactions use the prior summary plus newly retired messages.

Use a dedicated summarization call with tools disabled and a bounded output.
Treat transcript text as data, not new instructions. Start with the current model
route; evaluate a cheaper summarizer separately before changing routing. Budget
and meter summary calls. Reject error, empty, cancelled and length-truncated
responses. Bound overflow recovery to one compact-and-retry attempt, with no
reexecution of completed mutating tools. Oversized summary inputs require bounded
chunk summarization or an explicit pause; no endless retries.

### Tool output and cache behavior

Keep existing line cursors. Once context management is working, evaluate larger
read excerpts (for example 8–16 KiB per call) under an aggregate round budget.
The exact values should come from replay/evaluation. Make one layer responsible
for the visible structured result so JSON and continuation metadata stay valid.

For non-repeatable or expensive outputs, retain the original in the existing
app-scoped persistence path and return a private opaque result/artifact reference
with a bounded preview. Prefer existing read/range capabilities where possible.
If a result-range tool is needed, define its strict schema and app/run-derived
authorization before adding it; update runtime registration, Helpin catalog,
presets/custom-agent exposure and retrieval/retention policy together. Never
require reexecution of a mutating command just to recover its output.

After baseline compaction works, evaluate pruning old successful read outputs
to short receipts. Protect recent evidence, errors needed for repair, active
skills and unresolved interactions. Preserve original records and recoverability.
Prune in batches at measured savings thresholds: rewriting old prompt content
can invalidate prefix caches. Keep the system/tool prefix stable between context
changes and measure cached and uncached tokens separately. Do not globally
deduplicate reads across file changes or suppress legitimate repeated checks.

### Provider-native compaction

Add later behind an explicit provider capability. OpenAI supports server-side
compaction and a standalone compact endpoint. The standalone returned window is
canonical and must be replayed intact; it can contain retained items as well as
an opaque compaction item. Server-side stateless chaining must retain compaction
items, while previous-response-ID chaining has different pruning rules.
See [official compaction documentation](https://developers.openai.com/api/docs/guides/compaction).

Our current `NativeBlock` conversion only carries selected text/tool fields. It
must preserve provider-native compaction and required reasoning/state items
losslessly before this path can work. A saved response ID alone is insufficient.
Do not assume an OpenRouter route supports every OpenAI Responses feature.
Specify model/provider-switch compatibility and retain a portable recovery path.

## Delivery sequence

| Phase | Agent Runtime | Helpin | Completion gate |
| --- | --- | --- | --- |
| 1. Accounting and visibility | Persist per-request usage, summary usage category, cumulative checkpoints, context-size estimates, and failure usage | Correct reasoning settlement; idempotent checkpoint projection and budget cancellation | Replayed events and resumes never double-charge; failures retain observed usage; cancellation is observed before the next admitted request |
| 2. Portable compaction | Durable active-context checkpoints; resolved model limits; summary + retained groups; resume and one-attempt recovery | Pass validated context policy/model limits; show compaction lifecycle and failure state | Long single-turn and resumed runs stay bounded; history and approvals remain correct |
| 3. Useful tool reads | Coordinate cursor-aware output budgets; saved-result retrieval; protect structured outputs | Align changed tool contracts, visibility and retention | Fewer read rounds without missing content or increased total cost; retrieval remains tenant-scoped |
| 4. Tune and extend | Batch old-output pruning; optional provider-native compaction | Per-preset policy and rollout controls | Better cost/latency with no material quality regression across routes |

Phase 1 must distinguish synchronous runtime request admission from asynchronous
Helpin cancellation. Host cancellation alone cannot promise an exact spend cap.
Use a trusted runtime token limit or an explicit per-request budget admission
contract for tighter enforcement, reserving the next request's maximum allowed
output and checking cancellation before every provider call. Keep monetary
pricing authority in Helpin. Define resume and delegated-run budget ownership;
neither compaction nor delegation should create a fresh allowance implicitly.

Roll out additive storage/events and tolerant Helpin readers before enabling the
runtime writer/policy. Decide the shared `agent-runtime-go` event/schema release
if typed contracts change. Ship behind a native-context feature flag per app or
preset, then canary. Disabling auto-compaction should preserve the last valid
checkpoint and budget enforcement, rather than resurrecting full history.

## Verification and evaluation

Deterministic tests must cover long tool loops, multiple compactions, one huge
user turn, parallel tool groups, pending approvals, steering during compaction,
resume/restart, repeated checkpoint delivery, model changes, missing/zero usage,
huge initial prompts, large write arguments, JSON output validity, summary
truncation/failure, cancellation and non-shrinking recovery. Include a regression
that previously retained messages are summarized when they age out next time.

For reasoning billing, use a concrete inclusive-usage fixture: completion=100,
reasoning=20 must bill 80 ordinary output + 20 reasoning, not 140 output-equivalent
tokens. Preserve provider-specific cache-write semantics when normalizing usage.

Use recorded transcripts to measure context trajectories, repeated ranges,
serialized payload sizes and approximate replay savings. That is not sufficient
to establish task quality: run controlled end-to-end evaluations on documentation
and code investigation tasks, including facts in the middle of long source files,
changed requirements, multiple writes, and interrupted/resumed work. Compare
baseline, portable compaction, larger reads, and pruning separately.

Record provider input/output/cache usage, summarization overhead, peak context,
number of model calls, repeated-read rate, compaction reduction, latency, tool
errors, and factual completeness/citations in the resulting document. Require
bounded context, exact accounting and preserved execution invariants before
rollout; choose cost/quality targets from the measured baseline rather than
claiming an unmeasured savings percentage.

## Remaining decisions

- Obtain the reported run ID and split input, cached input, output and reasoning;
  inspect whether repeated reads, many tiny pages, large write payloads, resumes
  or delegated work account for the total.
- Select the authoritative model-limit source and unknown-model behavior.
- Choose initial per-preset soft context limits from the evaluation.
- Confirm saved-result retrieval can use existing storage and read capabilities
  before introducing a new tool or host callback.
- Define shared budget ownership for parent/delegated runs separately from
  context size. Compaction is not a substitute for task-level spend control.
