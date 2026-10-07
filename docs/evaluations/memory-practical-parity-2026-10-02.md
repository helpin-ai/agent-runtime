# Practical desktop memory parity — 2026-10-02

The **scoped comparison passes**: Go and pinned Hindsight answered **6/6**
complete-history LongMemEval questions and **3/3** paired feature cases correctly.
This meets the requested close functional match for the tested desktop profile.
Cloud integration is deferred. The full 500-question gate remains unrun.

| Category | Question | Go | Hindsight |
| --- | --- | --- | --- |
| single-session-user | 001be529 | Correct | Correct |
| single-session-assistant | 0e5e2d1a | Correct | Correct |
| multi-session | 00ca467f | Correct | Correct |
| temporal-reasoning | 08f4fc43 | Correct | Correct |
| knowledge-update | 01493427 | Correct | Correct |
| single-session-preference | 06878be2 | Correct | Correct |

Each benchmark question retained all source conversations and distractors. The
pilot contains 286 conversation documents and covers all six categories with
one deterministic selection per category. Retained ingestion was reused where
unchanged; every retrieval, answer and judgment was recomputed after the final
code changes. The independent local judge was GPT-OSS 20B with high reasoning;
manual review confirmed the expected answers. In particular, the earlier asylum
precision issue now answers “over a year” correctly on both backends.

The feature cases ran live extraction, consolidation and Reflect for a drink
preference, a move from Paris to Stockholm, and an unavailable date of birth.
Both implementations answered all three correctly. Mental-model refresh and
forgetting were exercised in the preference case. Go produced one merged address
observation and Hindsight two, preserving the same answer. Go cleared a mental
model derived from a deleted source; Hindsight retained its cached summary.

Inference was entirely local: Qwen 3.6 35B-A3B for extraction/reflection/answers,
Nomic Embed Text v1.5 at 768 dimensions, and GPT-OSS 20B for judging. Both used
RRF without neural reranking. Reference source:
`017b3f5d888d67341e70f43c102cb8155bb9f51e`; AMB:
`f618ed7b1f0eb9cad7b42e876f91a42f0eadb150`.
Frozen Go evaluator SHA-256:
`9a5967adff2e468937436fd22538f238d7dd96ade8041eb6a52599ef2a157bd3`.

The final changes provide o200k_base budgets, relative English temporal queries,
exact duplicate suppression, smaller-batch consolidation retries, optional bank
capacity, host Reflect directives, context-exhaustion synthesis, and mental-model
snippet/read behavior with checked citations. Focused Go race tests, runtime and
tool tests, vet, application builds, 12 Python tests, cgo-free memory tests, and
Windows/Linux compilation passed.

SQLite keyword ranking, literal entity matching and bounded synthesis remain
implementation differences. Compound tag policies, nondefault dispositions,
source expansion and multimodal ingestion are outside this profile. A selected
pilot supports the scoped result above; it does not establish full-corpus
statistical equivalence or every upstream configuration.

Evidence: [full-history pilot](memory-local-practical-parity-2026-10-02.json),
[paired features](memory-local-practical-features-2026-10-02.json),
[implementation/configuration](../memory.md),
[historical evaluation record](memory-parity-2026-10-02.md).


## Full-run follow-up — stopped 2026-10-03

The user stopped the expanded local benchmark after **16/500** completed
full-history questions spanning all six categories. Go was judged correct on
**16/16** and Hindsight on **15/16**, with no completed-case execution errors.
One additional case was partially ingested and is excluded from scores.
Completed results and retained databases are preserved for resuming.

Together with the three paired consolidation/Reflect feature cases and focused
race, integration and portability checks, this is useful evidence for continued
desktop integration and dogfooding. It remains a small selected sample and does
not establish full-corpus statistical parity or production reliability.
The [stopped-run evidence](memory-local-full-benchmark-stopped-2026-10-03.json)
preserves the results and exact profile.
