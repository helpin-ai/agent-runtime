# Hindsight parity evaluation — 2026-10-02

The final [practical desktop comparison](memory-practical-parity-2026-10-02.md)
passes its bounded scope: both implementations scored 6/6 full-history questions
and 3/3 paired feature cases. Full 500-question statistical parity remains unrun.
This record preserves earlier failures, fixes and superseded profiles.

Reference: Hindsight `017b3f5d888d67341e70f43c102cb8155bb9f51e`, launched
from its clean source checkout with pgvector/PostgreSQL 18.1. Inference was
local LM Studio: `qwen/qwen3.6-35b-a3b` extraction, Nomic Embed Text v1.5
768-dimensional embeddings, no neural reranking (upstream RRF passthrough),
observations disabled. The deployment used the unmodified pinned source.
Standalone Go smoke reports mark the deployment unverified because they cannot
attest an external server. The managed corpus runner separately checks the
source and launches the reference process itself.

| Live smoke run | Go evidence recall / MRR | Hindsight evidence recall / MRR |
| --- | --- | --- |
| Before retrieval fixes | 2/3 / 2/3 | 3/3 / 3/3 |
| After retrieval fixes | 3/3 / 3/3 | 3/3 / 3/3 |

The employment query returned a language preference in the initial Go run.
Corrections: Porter stemming; bounded semantic graph seeds excluded from graph
discoveries; upstream strict schema descriptions and user-message formatting.
A regression test reproduces the failed employment query.

Artifacts: [before](memory-smoke-before-fixes-2026-10-02.json),
[after](memory-smoke-after-retrieval-fixes-2026-10-02.json).

A six-category LongMemEval **oracle diagnostic** was started using pinned AMB
`f618ed7b1f0eb9cad7b42e876f91a42f0eadb150` and the 500-question cleaned
S corpus, SHA-256 `d6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442`.
Oracle mode omitted distractors. Answer and judge were the same local Qwen
model, so these judgments are provisional.

- `001be529` (single-session user): both backends retrieved the source and were
  judged correct. One-session ingestion took 76.5 seconds locally and 87.4
  seconds in Hindsight.
- `0e5e2d1a` (single-session assistant): Go ingestion failed because an event
  omitted dates. No paired score was produced. Upstream allows unknown dates
  and applies relative-date/point-event fallbacks; those behaviors were then
  ported and regression tested.

The diagnostic was interrupted before completing the remaining categories to
apply the date fix and switch inference following the user's OpenRouter offer.
The [interrupted report](memory-longmemeval-interrupted-pilot-2026-10-02.json)
is historical evidence and does not measure the final date-handling code.
No full-corpus score is available yet. Independent hosted judgments are recorded below.

`scripts/memory/parity.py` provides the repeatable full-history comparison and
an explicit acceptance gate. A pilot, oracle history, model error, unverified
reference, duplicate question, missing category or self-judge cannot pass it.
Both Go adapters currently budget serialized JSON by UTF-8 bytes; this differs
from upstream's tokenizer and must be considered when interpreting scores.

Feature gaps include observation consolidation, Reflect, mental models,
multimodal ingestion, exact keyword scoring/entity resolution, exact tokenization,
complete temporal inference and temporal-graph expansion.
Matching a raw-fact retrieval score cannot establish these capabilities.


## OpenRouter full-history facts-only pilot

The corrected six-category diagnostic completed with live extraction and
1536-dimensional `openai/text-embedding-3-small`. Extraction and answer model:
`openai/gpt-6-luna`; independent judge: `deepseek/deepseek-v4.1-flash`.
Both backends retained all source sessions with eight document workers;
observations and neural reranking were disabled. This is a **facts-only**
profile: AMB's normal Hindsight adapter additionally includes source chunks.

| Category | Go correct | Hindsight correct | Go source recall | Hindsight source recall |
| --- | --- | --- | --- | --- |
| Single-session user | yes | yes | 1 | 1 |
| Single-session assistant | no | no | 0 | 1 |
| Multi-session | yes | yes | 1 | 1 |
| Temporal reasoning | yes | yes | 1 | 1 |
| Knowledge update | yes | yes | 1 | 1 |
| Single-session preference | yes | yes | 1 | 1 |

Both scored **5/6**. The paired answer differences were zero on these six
cases, but this sample cannot establish general noninferiority. The gate failed
because all 500 questions have not completed. Actual charged model cost was
$0.43832543; this excludes earlier interrupted diagnostics. A matching six-case
pilot does not establish full feature parity.

[Full pilot evidence and judgments](memory-openrouter-small1536-facts-pilot-2026-10-02.json).
The binary hash in the report identifies the evaluated version. Default recency
scoring and source-chunk recall were added afterward, so this report does not
measure those additions.

The subsequent latest-scoring **live smoke** returned all three expected source
documents for both Go and Hindsight (evidence recall and MRR 1.0). It uses the
same hosted model/embedding profile; [smoke artifact](memory-openrouter-small1536-scoring-smoke-2026-10-02.json).
This also remains a diagnostic. Unit checks compare 128 chunking cases and 105
recency cases directly with outputs from the pinned upstream implementation.
Source-chunk storage, Unicode budgeting, scope isolation, forget, migration and
snapshot restoration have regression coverage; a paired source-chunk diagnostic
is the next live check.


## Verified source-chunk full-history pilot

The next live run used source chunks plus default combined recency scoring.
A live preflight verified the pinned reference actually returned source chunks
using nested HTTP `include.chunks`; an earlier SDK-style request was rejected
as an invalid comparison and excluded from these scores.

Go scored **4/6**, Hindsight **5/6**. Go lost the assistant study-count and
multi-session appointment-count cases; Hindsight lost the temporal case that
Go answered correctly. The paired mean difference was -1/6, with a bootstrap
interval extending below the five-point margin. This diagnostic fails the
quality gate, in addition to being only six cases. Both used complete histories,
1536-dimensional small embeddings and the independent DeepSeek judge.

[Source-chunk pilot evidence](memory-openrouter-small1536-source-chunks-pilot-2026-10-02.json).
The local candidate pool was still 100 in this evaluated binary, versus the
reference's high-budget retrieval. Afterward the runner default was aligned to
1000 candidates and PostgreSQL English stop words were added to local query
filtering. A targeted live regression checks the two lost cases next. These
changes do not by themselves establish parity or resolve the missing features.


The two-case targeted regression after stop-word/candidate-limit alignment
completed without errors. Go answered both questions correctly (2/2); Hindsight
answered the appointment-count question correctly but missed the study count
(1/2). Both retrieved all expected sources on both cases. Cost: $0.097246877. [Targeted regression evidence](memory-openrouter-small1536-retrieval-regression-2026-10-02.json).
This verifies those cases on the revised binary; it must not be combined with
older runs to manufacture a six-case score or a full-corpus parity result.
Windows/amd64 and Linux/amd64 memory test binaries also cross-compile with cgo
disabled; execution tests were run on macOS.


## Extraction recovery and incomplete six-case rerun

The subsequent aligned six-case run completed five questions: Go and Hindsight
each answered 4/5 correctly. The temporal case failed during Go ingestion after
three invalid structured outputs; its incomplete pair is excluded from accuracy.
This run therefore cannot establish even a complete six-case score. Charged cost
was $0.247843737. [Incomplete six-case evidence](memory-openrouter-small1536-incomplete-six-pilot-2026-10-02.json).

The pinned upstream output-retry split policy was then ported, with 56 direct
differential cases. Recovery makes up to three structured-output attempts per
fragment and splits at most three levels. Refusals and transport errors remain
fatal. Exhaustion returns an error and preserves existing document facts, rather
than silently omitting a source fragment. Tests force split recovery, verify causal
index offsets and original chunk provenance, and verify bounded failure.

A targeted live temporal rerun ingested all 47 full-history documents successfully:
Go answered correctly, Hindsight did not (1/1 versus 0/1). Charged cost was
$0.066713227. [Adaptive temporal regression](memory-openrouter-small1536-adaptive-temporal-regression-2026-10-02.json).
This artifact does not count adaptive split events, so it establishes successful
ingestion of this case with the revised binary, not that a split occurred live.

All hosted diagnostics above serialize recalled Go evidence as JSON before
answer generation. They use the pinned benchmark answer and category judge
prompts, but do not reproduce AMB's normal markdown result formatter. Together
with the declared retrieval, tokenizer and observation differences, this limits
comparison with published benchmark numbers. The 500-question gate remains
unpassed and full Hindsight feature parity remains unestablished.


## Complete adaptive-recovery six-category pilot

The fresh six-case full-history run completed every question without ingestion
errors. Go answered **5/6** correctly and Hindsight **6/6**; Go missed the
single-session assistant study count. The paired accuracy difference was -1/6
and the bootstrap interval was [-0.5, 0.0]. The gate failed both the
500-question requirement and the five-point noninferiority bound. Charged cost
was $0.448641481. [Current adaptive-recovery evidence](memory-openrouter-small1536-adaptive-six-pilot-2026-10-02.json).

This run measures the current Go recovery, chunking, retrieval and scoring
implementation with the historical JSON answer context. The runner's subsequent
markdown-context alignment is separately checked against the pinned AMB formatter
and its deduplication fixture; these scores do not measure that revised answer
context. Repeated study-count judgments flipped between runs, so earlier targeted
successes are not evidence of stable quality. No parity claim is warranted.

The relevant Go package, runtime/tool integration and race checks pass. The
backend and host commands also pass cgo-free checks; current memory test binaries
cross-compile for Windows/amd64 and Linux/amd64. Python evaluation tests pass.
These checks establish implementation behavior, not memory answer quality.


## AMB-context extraction coverage diagnostic

The targeted full-history study-count run now used the original pinned AMB
markdown formatter, including its chunk deduplication, and recorded every
document's retain result. Both backends answered incorrectly (0/1 each).
Go retained zero facts from the expected study source, and nine of the 46
documents produced zero facts overall. Its raw source remained stored, but
zero-fact documents do not enter the current fact-based recall path. Hindsight
retrieved the expected source but answered 20 instead of the gold count 38.
[Aligned-context extraction diagnostic](memory-openrouter-small1536-amb-context-regression-2026-10-02.json).
Charged cost was $0.046587097; total audited hosted diagnostic spending was
$1.933206530, excluding tiny earlier unmetered probes.

A direct comparison with the pinned default extraction builder confirmed that
the copied Go system prompt and strict schema equal upstream's defaults. The
prompt SHA-256 is `50abdf5b1ff2a13b4342efd0fc9b888f433a5eb64f7e46803f12401a511fe923`.
This does not establish identical requests or model outputs: date serialization,
metadata ordering, provider behavior and inference scheduling can differ.
No root cause beyond observed zero-fact extraction is established. Next quality
work must address extraction coverage and evaluate the complete corpus; retrieval
tuning alone cannot recover a source that contributed no indexed facts.


## Local-only work (2026-10-02)

Hosted inference was stopped at the user's request. The new profile uses local
LM Studio `qwen/qwen3.6-35b-a3b` for extraction/answers, Nomic Embed Text v1.5 at
its native 768 dimensions, and local `openai/gpt-oss-20b` as an independent judge.
The earlier 1536-dimensional hosted reports remain separate historical evidence.
Qwen's `reasoning_effort: none` is required here to obtain final extraction JSON;
the earlier chat-template flag alone returned empty final content. No hosted
credentials are passed to this local profile.

The 30-query pooled gold-source diagnostic retained 42 conversations: Go produced
521 facts with no zero-fact source; Hindsight stored 609 facts. Both achieved
28/30 expected-document recall (93.3%). Mean reciprocal rank was 0.6473 in Go and
0.7156 in Hindsight. Go ingestion took 1177.337 seconds; Hindsight took 919.264
seconds at two retain workers. This pools gold conversations without full
per-question distractor histories and does not judge final answers. It cannot
establish parity. Its executable also predates the expanded keyword-index inputs
and later matching document-ID metadata in both extraction requests.

A separate retrieval-only replay supplied the same 521 Go-extracted facts, source
chunks, entity identities/links and stored vectors to an isolated verified pinned
Hindsight bank. Stored vector float32 digests matched. Both again achieved 28/30
source recall. Reference query vectors are recomputed using the same local Nomic
model; they are not captured/replayed query vectors. This bypasses Hindsight
extraction and entity resolution and remains diagnostic only. See
[machine-readable diagnostic](memory-local-coverage-and-shared-retrieval-2026-10-02.json).

The clean synthetic local feature smoke processed two facts into two observations,
answered Alice's drink-preference question through a grounded Reflect loop,
refreshed a mental model, and cleared its generated content after a supporting
source was forgotten. See [feature smoke](memory-local-feature-smoke-2026-10-02.json).
This checks feature wiring, not consolidation/Reflect quality parity. The local
judge accepted the exact gold answer and rejected an unrelated answer using the
pinned AMB single-session-user rubric in a preflight.

A complete-history six-category pilot is running in the ignored directory
`.local/evaluations/local-nomic768-date-parity-20261002/`, with 286 conversations, source chunks,
AMB markdown answer context, two retain workers, independent local judging and a
frozen evaluator. The executable SHA256 is
`c8691fe6ea56e762cf46d0b484dc141e9a277ea72223cf00489c614bf5fbd442`.
Its report is saved after each case and per-case SQLite files are preserved for
resume. No completed pilot accuracy is reported yet. A continuation runner is attached to the pilot and will expand to 30 full-history
questions, then all 500, reusing completed cases. It stops on incomplete cases or
operational failures; neither later stage has completed yet. At observed local
throughput, these are long runs; no small diagnostic is being promoted to parity.

Current consolidation, observation search, Reflect and mental-model implementations
reuse pinned upstream prompts/schemas but have documented candidate retrieval,
deduplication, freshness/indexing, directives and synthesis gaps. Full feature
parity remains false. Encrypted checkpoint save/restore/prune contracts are
implemented and tested with in-memory and signed-URL HTTP providers; no actual
cloud bucket or production checkpoint authority has been configured or uploaded to.


Before scaling, schema serialization was aligned too: extraction/consolidation
schemas and reflection tool definitions preserve upstream property order rather
than re-encoding nested maps in alphabetical order. Although order is not part
of JSON Schema semantics, constrained model generation can use it. The original
local full-history pilot was interrupted before any paired judgments and retained
as a superseded diagnostic. The corrected pipeline uses fresh banks/databases and
a new frozen executable; it does not reuse earlier extracted facts. Historical
coverage and feature smoke reports predate this alignment and remain labeled as
such. The wire-level extraction regression now checks the pinned property order.


The clean local feature smoke was repeated after the schema-order fix and passed
extraction, consolidation, grounded Reflect, mental-model refresh and source
forgetting again. See [ordered-schema feature smoke](memory-local-feature-ordered-smoke-2026-10-02.json).
Observation freshness is now conservative: pending unconsolidated evidence marks
observations stale and requires raw recall before finalizing. This update affects
the initial reflection feature; the paired benchmark continues using the declared
raw-fact profile with observations disabled.


The ordered-schema full-history pilot exposed another real difference: Go
rejected invalid model dates, failing conversations in four categories before
paired judgments. Pinned upstream `_parse_datetime` catches invalid ISO dates and
returns unknown. The Go extractor now follows that policy, plus common calendar,
ordinal/week and ISO-clock forms, verified against 53 direct upstream fixtures.
Invalid dates preserve the fact text; they do not invent dates or silently drop
facts. Missing-date fallbacks are applied only to absent fields, matching upstream
conversion order. The exact previously failing conversation
`001be529_0b64c6cb_2` now retains successfully with 12 facts using local inference;
see [date regression](memory-local-date-regression-2026-10-02.json).

The failed pilot is retained as a superseded diagnostic. The current date-compatible
pipeline restarts with fresh banks/databases and the frozen executable above.
No completed accuracy or parity claim is derived from either interrupted pilot.

### Complete-history failure and subsequent ports

The frozen date-parser pilot completed `001be529` with Go judged incorrect and
Hindsight judged correct. Both extraction and dense/keyword retrieval found the
asylum fact. Go nevertheless excluded that fact from the final context: its
passthrough scoring normalized across the entire fused pool instead of applying
Hindsight's default 300-candidate cap first. The implementation now applies that
cap independently of each arm's limit. Recalling the same persisted history
places the asylum fact at rank 28 and includes it in the serialized budget.

The focused answer check is saved in
`memory-local-candidate-cap-diagnostic-2026-10-02.json`. The local judge accepted
"one year" as "over a year"; this discrepancy is explicitly recorded and this
answer is **not** counted as precision-correct parity evidence. The improvement
proves evidence inclusion, not correct duration interpretation.

Consolidation now performs per-fact related-observation recall with interleaving,
unioned candidates and dated supporting source evidence. Semantic near-twin
adjudication uses pinned prompts/schema, the 0.97 default threshold and top-five
probe, with atomic source-preserving create/update folds. Observation retrieval
adds scoped keyword search, source-fact/entity/source-fact graph traversal,
temporal fusion and proof boosts. Reflect now forces raw retrieval after
observations and reserves its last evidence-bearing turn for done.

Direct differential fixtures exercise pinned fusion, scoring at four pool sizes,
and consolidation evidence serialization. Functional tests check bank isolation,
source-entity traversal, semantic-fold provenance, forget invalidation, failed
inference retry, candidate capping and final-turn grounding. These are component
checks. They do not prove end-to-end consolidation or Reflect quality parity.
The running full-history pipeline remains frozen at its original executable;
its scores must not be attributed to these later changes.

The fresh updated Go feature run is saved in
`memory-local-feature-hybrid-fresh-smoke-2026-10-02.json`: two input facts,
two created observations, a correct three-step Reflect answer, a fresh mental
model, and invalidation/cleared generated content after forgetting a proof.

`memory-local-paired-features-2026-10-02.json` compares the same two source
utterances and timestamps with a separate observation-enabled bank on the
pinned live Hindsight service. Its consolidation operation completed, produced
two observations, and Reflect answered jasmine tea without sugar. The independent
local judge marked both Go and Hindsight answers correct. Hindsight's trace used
one observation search before final synthesis; Go checked raw evidence as well.
This is one paired feature diagnostic, not comprehensive behavioral parity.

A local judge precision preflight with `reasoning_effort=high` accepted the gold
answer and rejected both "exactly one year" and "two days" for "over a year".
The three preflight cases are saved in
`memory-local-judge-precision-high-2026-10-02.json`. The longer original answer
still received an overly permissive verdict during the actual comparison;
passing the short preflight therefore does not remove the documented precision
limitation or certify all judgments.

The latest frozen comparison is now
`.local/evaluations/local-nomic768-highjudge-parity-20261002/`, executable SHA256
`5e85695fac3943bbdcbd014592da86a279cc409e8482a174e59b7037a1e079c6`, reference
port 18893. It uses the final graph-window ordering, upstream Reflect defaults
of 10 iterations/100000 context tokens, and the high-reasoning local judge.
Retained SQLite banks and the reference's existing documents are reused with
explicit report provenance; every answer and judgment is recomputed. The prior
date-parser and initial hybrid runs are retained as historical evidence. The
6/30/500 pipeline remains active. Its raw recall evaluation disables observations
on both sides; the feature diagnostic above is separate. Neither result may be
presented as completed full Hindsight parity.


## Final practical desktop profile

The user accepted close functional parity and deferred cloud integration. The
final build adds o200k_base token budgets, relative English date windows,
whitespace-normalized exact duplicate suppression, bounded consolidation batch
retries and optional per-bank observation capacity. Reflect accepts host
directives, synthesizes from recent evidence when context fills, and requires a
full read before citing mental-model snippets. Refreshed model embeddings
include summary content. Literal normalized entity matching and SQLite FTS5
scoring are retained rather than adding a fuzzy registry or PostgreSQL rank
emulation. Banks provide host-selected source isolation; upstream compound tag
policies are outside this desktop profile.

Frozen evaluator SHA-256:
`9a5967adff2e468937436fd22538f238d7dd96ade8041eb6a52599ef2a157bd3`.
Source hashes and retained-state provenance are stored under
`.local/evaluations/local-practical-parity-20261002/`. Extraction and embeddings
are unchanged from the retained histories; all final retrievals, answers and
judgments are recomputed. The sixth case completes its partially retained
Hindsight history before scoring. No oracle histories or previous judgments are
used. The automatic 6→30→500 expansion was stopped for this bounded profile;
the existing 500-question gate is preserved.

The paired [feature report](memory-local-practical-features-2026-10-02.json)
completed: both implementations answered **3/3** preference, changing-address
and abstention cases correctly under an independent local GPT-OSS judge and
manual review. Both ran live extraction, consolidation and Reflect. The
preference case also refreshed a mental model and deleted a supporting source.
Go produced one merged observation for the address transition, while Hindsight
produced two; both answered the current and prior residence correctly. Go
cleared the source-dependent mental model on deletion; Hindsight retained its
cached summary in this run. This stronger local invalidation is intentional.

Focused Go race tests, runtime/tool memory tests, vet and application builds
passed. Twelve Python evaluator tests passed. The final memory package runs
without cgo and cross-compiles for Windows and Linux. Ten token-count fixtures
match pinned Hindsight's bundled o200k_base tokenizer, including Unicode,
JSON and special-token literals. Regression checks cover capacity overflow,
batch retries, exact duplicates, date windows, context exhaustion and snippet
citation permissions.

The final six-category complete-history comparison completed: both backends
scored **6/6**, with no failed cases. Manual review matches the expected answers.
The only remaining reason the original gate fails is its requirement to finish
all 500 questions. The [scoped report](memory-practical-parity-2026-10-02.md)
records the practical result and deliberate differences; it does not replace the
500-question statistical gate or establish every upstream configuration.
