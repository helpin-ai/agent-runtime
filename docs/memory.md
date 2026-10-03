# Local long-term memory

Agent Runtime has an optional memory module with `Retain`, `Recall`, and `Forget`
operations. It is separate from native run checkpoints and context compaction.
The public Go package is `github.com/helpin-ai/agent-runtime/memory`.

This is the first Hindsight porting milestone, not a claim of equivalent answer
quality. It supports text ingestion and retrieval of world and experience facts.
Consolidation, grounded `Reflect`, and host-curated mental-model refresh now have
initial local implementations. They still differ from the upstream engines.

## Storage and inference

The embedded backend uses the same pure-Go SQLite driver as the local CLI.
It stores source documents, extracted facts, embeddings, entities, semantic and causal
links in one database with WAL enabled. Each bank records its embedding model
identity and dimensions; incompatible models fail instead of mixing vectors.

Retain preserves the source document and atomically replaces its derived facts.
Identical retries are idempotent. Extraction and embedding finish before the
storage transaction starts, so a failed model call preserves the previous facts.
Concurrent replacements use document revisions to detect conflicts.

Recall combines four arms with Hindsight's reciprocal rank fusion (`k=60`):

- Exact cosine similarity, computed in Go over the bank's embeddings.
- FTS5 BM25 with a separate corpus per bank, a Porter/Unicode tokenizer, and quoted query terms.
- One-hop shared-entity, weighted semantic-link and directed causal-link expansion from bounded semantic seeds.
- Event/mention date overlap with a supplied or inferred time window.

A supplied time window boosts the temporal arm; it does not exclude results from
the other arms. Inference recognizes ISO date ranges and today/yesterday/tomorrow.
For richer temporal expressions, supply an explicit window and query timestamp.
An optional cross-encoder reranker orders the fused candidates before budgeting.
Configured reranker failures return errors; they do not silently change modes.

The extraction prompt is copied from Hindsight's concise mode at commit
[`017b3f5`](https://github.com/vectorize-io/hindsight/tree/017b3f5d888d67341e70f43c102cb8155bb9f51e).
Its original MIT notice is preserved under `memory/upstream/LICENSE`.
The strict output schema and its descriptions are generated from that revision.
Attachment indices are ignored by this text-only implementation. Extraction
uses upstream’s 3,000-character text, JSON conversation and JSONL chunking
algorithms and rejects incomplete responses before persisting anything. Invalid
structured output receives at most three attempts per fragment. Malformed or
truncated output may trigger up to three levels of adaptive splitting, using
upstream's 500-character minimum and conversation-aware split policy. Refusals
and transport errors return immediately. Failed attempts never append partial
facts; exhausted recovery returns an error and preserves prior document memory.
Unlike upstream's last-resort drop, no failed fragment is silently omitted.
Missing event dates remain unknown; the
upstream relative-date fallback and point-event end fallback are applied when
supported by the extracted text.

Inference is supplied through `Extractor`, `Embedder`, and `Reranker` interfaces.
The included provider uses OpenAI-compatible `/chat/completions` (JSON Schema
structured output), `/embeddings`, and optionally a vLLM-compatible `/rerank`
endpoint. Storage is local; inference is fully offline only when these endpoints
run locally. The app must arrange model installation and lifecycle separately.

## Enable memory tools

Memory is disabled by default. API, worker, and local CLI entrypoints accept:

```sh
export AGENT_RUNTIME_MEMORY_BACKEND=sqlite
export AGENT_RUNTIME_MEMORY_SQLITE_PATH=/absolute/path/to/memory.db
export AGENT_RUNTIME_MEMORY_MODEL_URL=http://127.0.0.1:8000/v1
export AGENT_RUNTIME_MEMORY_EXTRACTION_MODEL=your-extraction-model
export AGENT_RUNTIME_MEMORY_EMBEDDING_MODEL=your-embedding-model
# Optional explicit output dimensions (validated and included in model identity):
export AGENT_RUNTIME_MEMORY_EMBEDDING_DIMENSIONS=1536
# Optional retrieval pool (default 100; high-budget reference profile uses 1000):
export AGENT_RUNTIME_MEMORY_CANDIDATE_LIMIT=1000
# Optional: stable identity across model-server moves. Change on model changes.
export AGENT_RUNTIME_MEMORY_EMBEDDING_MODEL_ID=embedding-model-and-revision
# Optional when the provider needs authentication:
export AGENT_RUNTIME_MEMORY_MODEL_API_KEY=your-key
# Optional separate embedding server and credential (e.g. local embeddings):
export AGENT_RUNTIME_MEMORY_EMBEDDING_URL=http://127.0.0.1:1234/v1
# Only set if that embedding server needs authentication:
export AGENT_RUNTIME_MEMORY_EMBEDDING_API_KEY=embedding-key
# Optional request timeout for slower local inference (default 2m):
export AGENT_RUNTIME_MEMORY_MODEL_TIMEOUT=15m
# Optional provider sampling/reasoning controls (core request fields protected):
export AGENT_RUNTIME_MEMORY_EXTRACTION_OPTIONS='{"temperature":0,"reasoning_effort":"none"}'
# Optional when the model server implements /rerank:
export AGENT_RUNTIME_MEMORY_RERANK_MODEL=your-cross-encoder
```

The service registers `memory_retain`, `memory_recall`, and `memory_forget`.
SQLite also registers `memory_reflect`; its configured extractor must implement
`ReflectionModel`, as the included provider does.
An agent must allowlist the desired tools and opt into memory in its existing
execution configuration:

```json
{
  "allowed_tools": ["memory_retain", "memory_recall", "memory_forget"],
  "execution_config": {
    "memory": {"enabled": true, "bank_id": "user-123"}
  }
}
```

For reusable agent definitions, omit the fixed `bank_id` and supply
`metadata.memory_bank_id` when starting each run. The authenticated host must
authorize that bank for the caller. There is no default bank. A fixed agent bank
cannot be overridden by run metadata, and tools do not accept model-selected app
or bank IDs. Storage always scopes reads and writes by both app and bank.

Retain is classified as a routine mutation, recall as a read, and forget as a
destructive mutation. Existing agent approval policies continue to apply.
Memory is treated as retrieved evidence, not as authoritative instructions.
Raw conversations are not automatically retained: the host or agent must invoke
retain explicitly. Runtime tools attach trusted run and agent provenance.

For the standalone local CLI, enabling a backend exposes these tools and selects
a bank using the canonical repository path. Host-connected CLI runs do not
automatically receive this local memory authorization. A desktop host can use
the public Go backend directly or register its own memory-enabled agents.

Use embedded SQLite for a desktop/profile or a single-node deployment. Separate
worker-local files do not share memory. For a distributed deployment, point all
workers at the reference service until a shared storage backend is implemented.

## Hindsight reference backend

```sh
export AGENT_RUNTIME_MEMORY_BACKEND=hindsight
export AGENT_RUNTIME_MEMORY_HINDSIGHT_URL=http://127.0.0.1:8888
export AGENT_RUNTIME_MEMORY_HINDSIGHT_API_KEY=your-key
```

The adapter uses upstream's synchronous retain, recall, and document deletion
endpoints. It hashes the app/bank pair into a namespaced Hindsight bank, and hashes
document IDs into safe remote identifiers while preserving the original ID in
metadata. `memory.BankName(scope)` returns the name for administration.

Hindsight retain does not report extracted fact counts, so its `FactCount` is -1.
Its recall ordering is preserved without inventing comparable scores. Reranking
status is omitted when the server does not expose it. The reference service owns
its model configuration and deletion semantics; the local tombstone guarantee
does not extend to upstream.

## Forget and checkpoint

Local `Forget` removes a source document's content, facts, FTS rows, and graph
links. A tombstone prevents concurrent extraction or a retried import from
resurrecting the same document. Reimport requires a new document ID. Tombstones
retain only the scoped identifier and revision, not the source content.

`(*memory.SQLite).Snapshot(ctx, destination)` creates a consistent standalone
database using `VACUUM INTO`, without requiring the live WAL file. It refuses to
overwrite existing files and creates the output with private permissions.

The raw snapshot is plaintext. `EncryptedSnapshot` encrypts a consistent snapshot
with age before any upload. `SaveCheckpoint` sends only encrypted bytes through a
host-authorized `CheckpointStore`; `RestoreCheckpoint` authenticates/decrypts,
bounds the restored size, checks SQLite integrity/schema, and refuses to overwrite
an existing database. `PruneCheckpoints` expires older checkpoints. The application
supplies the storage adapter and persists the age identity through its key recovery
or OS keychain policy. No cloud bucket is configured by this library. Forgetting live memory does not erase historical
snapshots or guarantee secure erasure of old disk pages. Restoring an older
snapshot also restores its old deletion state. Do not implement multi-device
editing by copying active SQLite files between machines.

## Evaluate retrieval

Run the included deterministic smoke corpus without model calls:

```sh
go run ./cmd/memory-eval -recorded
```

To compare the embedded implementation and a Hindsight deployment, configure the
local inference variables and the reference URL, then run:

```sh
go run ./cmd/memory-eval -mode both -fixture memory/testdata/retain-recall.json
```

Both arms receive the same source documents, timestamps, queries, and budgets.
Evaluation uses a fresh, isolated bank and a temporary local database. Reference
banks remain available for debugging; remove the reported evaluation bank when
finished. Configure that reference bank/server with the same extraction,
embedding, and reranking models. Pin the upstream deployment yourself: the
report explicitly marks its commit as unverified rather than inferring it.

The JSON report includes evidence, per-query latency, document-level evidence
recall, and reciprocal rank. A fixture contains `documents` plus `queries` with
`expected_documents`. Optional recorded facts and embeddings support deterministic
local tests; they cannot be used to claim a live comparison against Hindsight.

The tiny smoke corpus checks wiring. It is not LongMemEval, does not score final
answers, and does not establish parity. A release-quality comparison still needs
LongMemEval, representative desktop conversations, and repeated live model runs.

Known differences to resolve during that work include SQLite keyword scoring,
literal normalized entity matching,
bounded graph expansion and complete temporal inference. Vector
retrieval scans the bank and grows linearly with its size. The default token
budget uses Hindsight's `o200k_base` encoding and includes serialized result framing; embedded callers can
supply a different `CountTokens` implementation. No ANN scale guarantee is made.

## Live LongMemEval comparison

`scripts/memory/parity.py` launches its own Hindsight reference process from a
verified, clean pinned checkout. It imports the pinned Agent Memory Benchmark
(AMB) LongMemEval loader, answer prompt, and category-specific judge prompts.
Use the reference checkout's Python environment with `rich` and `tiktoken`
installed. The separate AMB checkout must be at
`f618ed7b1f0eb9cad7b42e876f91a42f0eadb150`.

Configure local inference as above and `HINDSIGHT_API_DATABASE_URL` for an
isolated test PostgreSQL instance with pgvector. The reference uses RRF without
neural reranking and disables observations, matching the raw-fact comparison
profile. Each question has fresh banks, and `has_answer` labels are removed
before ingestion by the upstream dataset loader.

```sh
/path/to/hindsight/.venv/bin/python scripts/memory/parity.py \
  --hindsight-source /path/to/hindsight \
  --benchmark-source /path/to/agent-memory-benchmark \
  --dataset /path/to/longmemeval_s_cleaned.json \
  --answer-model your-answer-model --judge-model your-independent-judge \
  --include-chunks \
  --output /path/to/results.json
```

The default evaluates all 500 questions with complete histories. For a diagnostic
only, add `--per-category 1`. Adding `--history oracle` additionally ingests gold
sessions only and omits distractors. Its results cannot establish long-history retrieval
quality. The report preserves evidence, generated answers, judgments, source
commits, dataset checksum, and a paired bootstrap accuracy difference. It is
saved after each question. Model or backend failures are recorded as errors,
never converted into successful answers. Reference logs accompany the report.
Reference banks persist in the configured test database for inspection.

The quality gate requires 500 completed full-history questions, all categories,
a separate judge model, and a 95% paired confidence interval whose lower bound
is no worse than Hindsight by five percentage points. Exit status 2 means the
quality gate did not pass. This is a project acceptance rule, not Hindsight's
published benchmark configuration or a guarantee outside the tested corpus.
The report separately marks full feature parity false while consolidation/Reflect/mental-model differences, multimodal ingestion and
retrieval differences remain.

See [the live evaluation record](evaluations/memory-parity-2026-10-02.md) for
measured results and unresolved gaps.

### OpenRouter inference

The parity runner accepts `--env-file /absolute/path/to/.env.memory`; the file is
parsed with `python-dotenv` without executing shell code or interpolating values.
The repository ignores `.env.memory`. Put `OPENROUTER_API_KEY` there or expose it
to the runner's environment. The runner maps that key to the OpenRouter chat
endpoint only. It never copies the hosted credential to a separately configured
embedding endpoint or writes credentials into the evaluation report.

For the exact `https://openrouter.ai/api/v1` endpoint, runtime memory also
accepts `OPENROUTER_API_KEY` when its explicit model API key is unset. Custom
endpoints never receive that fallback credential. The selected profile is in
`scripts/memory/openrouter.env.example`.

Set an explicit extraction model plus the answer and judge model flags. Use
provider-specific reasoning options for the hosted models rather than LM
Studio's `chat_template_kwargs`. The same extraction settings are passed to
both memory implementations. Separate local embeddings remain supported.


The hosted diagnostic profile uses `openai/gpt-6-luna` for extraction/answers,
`deepseek/deepseek-v4.1-flash` as an independent judge, and
`openai/text-embedding-3-small` at 1536 dimensions. The runner loads `.env.pippi`
without executing shell code and never writes credentials into its reports.
Use `--retain-workers 8` for bounded parallel ingestion; each backend receives
the same concurrency, and recall begins only after all documents retain
successfully. The report records the concurrency, evaluated binary hash and
per-document retain results. SQLite reports extracted fact counts; Hindsight
returns -1 because its synchronous HTTP response does not expose those counts.

Semantic links use separate top-50 within-document and existing-bank neighbor
sets with cosine similarity at least 0.7. Exact scans replace upstream's ANN
probes. Entity, semantic and causal signals add within the graph arm; semantic
links traverse both ways and causal links follow their stored direction.
Schema version 2 adds link weights; version 3 adds source chunks; version 4 rebuilds keyword indexes with source context, entity names and event date signals. Version-1 causal links migrate with weight
1; existing facts need reindexing into a fresh bank to populate semantic links introduced here.


Final scoring follows upstream's default multiplicative recency adjustment
(365-day linear decay, 0.1 floor, ±10% boost) and temporal proximity adjustment.
Coarse month/year event spans use their end date and cap freshness at neutral.
Without a neural reranker, RRF rank supplies a base score from 1.0 down to 0.1.
Calibrated neural scores stay unchanged before these adjustments; logits receive
sigmoid normalization. Rerank inputs include source context and event dates.
Observation proof boosts and alternate decay configurations remain unsupported.


`RecallRequest.IncludeChunks` (tool argument `include_chunks`) returns deduplicated
source chunks keyed by each fact's `chunk_id`. `MaxChunkTokens` defaults to 16384
and budgets chunks separately from facts. Oversized chunks are truncated on
Unicode boundaries and marked `truncated`. Both Go adapters use `o200k_base` to budget
serialized JSON. Upstream budgets fact text separately, so framing remains a conservative difference. Upgrades
from older local schemas attach the preserved whole document as a fallback source
chunk without calling models. Fresh provider ingestion preserves upstream chunk
boundaries. Forget removes chunks and snapshot restore preserves them.

Use `--include-chunks` in the paired runner for facts plus source text. AMB's
standard Hindsight adapter includes chunks, so the earlier facts-only diagnostic
is a narrower profile, not a reproduction of published Hindsight scores.


English query stop words are filtered using the PostgreSQL reference vocabulary;
SQLite FTS5 scoring still differs from PostgreSQL's full-text ranking. The paired
runner now defaults its local candidate pool to 1000 to align with the reference
HTTP `budget=high`; the desktop library default remains 100 unless configured.
For a targeted regression, use repeatable `--question-id ID` flags. Targeted
runs and six-category pilots always fail the full-corpus parity gate.

The paired runner now defaults to AMB's pinned markdown answer context. It imports
the original Hindsight adapter's result wrapper, chunk deduplicator and formatter,
and validates a checked-in formatting fixture before making model calls. The
report records `answer_context_format`; `--answer-context json` reproduces the
earlier diagnostic profile. Earlier JSON-context pilot scores remain historical
and must not be treated as scores for the revised markdown-context profile.


### Fully local evaluation

`scripts/memory/local.env.example` selects the installed LM Studio Qwen extraction
model and Nomic embeddings at their native **768 dimensions**. This is a new bank,
separate from the earlier hosted 1536-dimension profile. Fact embeddings append the
pinned upstream date and entity hints; queries remain unaugmented. Automatic model
identities include this preprocessing version, preventing old vectors from mixing.
For this Qwen server, `reasoning_effort: "none"` produces final JSON content;
`chat_template_kwargs.enable_thinking=false` alone did not do so in live testing.

Add `--local-only --env-file scripts/memory/local.env.example` to the paired runner.
Local-only mode requires loopback inference endpoints and strips hosted credentials.
Use an independently installed local judge model, for example `openai/gpt-oss-20b`,
and Qwen for extraction/answers. Installing models and running the server remain
host responsibilities.

`--work-dir /absolute/path/to/evaluation-state` preserves each case's SQLite file
and bank identity. `--evaluator-binary /absolute/path/to/frozen-memory-eval` fixes
the executable being measured. `--resume` reuses completed paired judgments only
when the inference configuration, source/dataset checksums and executable hash
match. Expanding `--per-category 1` to `--per-category 5`, then to all 500 questions,
can reuse those completed cases. Incomplete cases retry their original bank;
local identical retains are idempotent. A resumed run still evaluates the missing
cases and cannot pass the gate until all 500 succeed.

For retrieval diagnosis, `scripts/memory/shared_retrieval.py` replays the same
Go-extracted facts, source chunks and stored vectors into an isolated pinned
Hindsight bank. It checks float32 vector digests after insertion. This bypasses
Hindsight extraction and entity resolution; query embeddings are independently
computed by the same local model on the reference side. Its results isolate
retrieval differences and cannot establish end-to-end answer-quality parity.


### Consolidation and reflection

`SQLite.Consolidate(ctx, scope, consolidator, batchSize)` processes up to 100
unprocessed facts with the pinned upstream default mission, processing rules,
input description and create/update/delete schema. The host schedules it; retain
starts no automatic background inference. All inference/embedding finishes before
one atomic write, guarded by a bank-state check. Consolidation retrieves related observations independently for each new fact,
interleaves the retrieval arms, unions that evidence, and supplies dated source
proofs using the pinned observation prompt projection. It uses a 512-token
observation budget per fact and a 256-token source budget per observation, measured
by the configured token counter. The configured provider also adjudicates near-twin
creates/updates above 0.97 cosine similarity against the five nearest existing
observations, using the pinned merge/keep prompt. Folds preserve all source proofs
and run under the same atomic bank-state check. Custom consolidators can implement
`ObservationReconciler` to opt into this adjudication. Tags, capacity policies,
exact-duplicate reconciliation and upstream adaptive batch scheduling remain gaps.
`SearchObservations` now fuses semantic, scoped FTS5 keyword, source-entity graph
traversal and temporal arms, with optional neural reranking and upstream proof-count
boosts. Upstream PostgreSQL keyword scoring, entity resolution and full temporal
parsing still differ. Pending unconsolidated facts conservatively mark observations
stale.

`SQLite.Reflect(ctx, scope, request, reflectionModel)` runs a bounded native tool
loop: fresh mental models first when available, observations next, a forced raw-fact check after observations, and a `done` call with checked references. Citation arrays are normalized
only against retrieved IDs or their recorded supporting fact IDs. Unknown IDs
fail. A changed bank during inference rejects the result. The final iteration is reserved for `done` when evidence has been retrieved.
Defaults match upstream at 10 iterations and a 100000-token context budget.
The default counter uses `o200k_base`. Host-supplied `ReflectRequest.Directives`
are appended to the system prompt. When accumulated context reaches its limit,
the loop removes older complete tool-call pairs and forces final synthesis from
recent evidence. Bank disposition, structured document output, expansion and
upstream map/reduce synthesis remain unsupported. Local-server compatibility uses required tool choice with a single
function for forced searches. If a server emits plain final prose, one additional
forced `done` call must provide valid references before it can become a result.
When a local server also ignores the forced call, constrained JSON arguments
are passed through the same function execution and citation checks. Plain prose
is never promoted to a grounded result.

`CreateMentalModel` records a host-selected ID, name and source question.
`RefreshMentalModel` derives its content through observations/raw facts, never
through its own prior summary, and persists complete supporting fact IDs.
`ReadMentalModel` and `SearchMentalModels` remain bank-scoped. Search currently
embeds the model's name/question and returns bounded full summaries; upstream's
snippet/read behavior and richer indexing are not reproduced. Any new retained
evidence conservatively marks all summaries in that bank stale. Replacing or
forgetting a supporting source clears the generated content and its proof IDs;
refresh is required before it can be searched again.

Schema version 5 adds observations/proofs/processed-fact state; version 6 adds
mental models and their proofs. Forget invalidates the entire derived observation
when any supporting fact is removed, then makes surviving proofs eligible for
reconsolidation. This avoids leaving source-derived details in a partly supported
sentence. Encrypted checkpoints preserve these states. Host-defined model topics
and names persist after their generated content is cleared.


`NewPresignedCheckpointStore(authority, client)` supplies a concrete encrypted
transfer adapter for host-issued signed PUT/GET URLs (including S3-compatible
storage). `CheckpointAuthority` authorizes the selected profile, lists checkpoint
metadata, and deletes checkpoints/object versions. Transfers refuse redirects
and omit signed URLs from transport errors. A local HTTP integration test verifies
that upload contains age ciphertext only and download restores the database.
No production authority or bucket is configured by the runtime.

`scripts/memory/local_pipeline.py` automates the complete-history 6 → 30 → 500
sequence using the same local profile, frozen binary and resumable case databases.
It saves a report for each stage. It stops expansion on incomplete cases or
operational errors so they can be diagnosed. A completed small pilot may proceed
to the next stage without passing the full-corpus gate; that gate still requires
500 successful paired judgments. Its `pipeline.json` status distinguishes finished
evaluation from established full feature parity.


The provider preserves upstream JSON Schema property order on the wire for
extraction/consolidation and native reflection tool definitions. This matters
for providers whose constrained output grammar chooses a field sequence from
the schema. The included wire regression guards against alphabetical map
re-encoding. Evaluation metadata also includes the same original document-ID
field in both backends, matching the reference adapter's provenance metadata.


Hosts that initialize the backend from runtime configuration can call
`SQLite.ConsolidateMemory(ctx, scope, batchSize)` to use its configured provider
without handling model credentials again. Consolidation and mental-model refresh
remain explicit host operations; they are not automatically run on every retain.


Extraction accepts the pinned upstream ISO-date behavior: invalid model date
strings remain unknown, preserving the fact rather than failing its entire
conversation. Calendar, ordinal/week dates, ISO offsets, microsecond truncation,
and midnight rollover are checked against 53 upstream fixtures. Relative-date and
point-event fallbacks apply to absent fields. Strict validation remains in place
for externally supplied reference response dates and request timestamps.


The per-arm `AGENT_RUNTIME_MEMORY_CANDIDATE_LIMIT` is independent of the fused
candidate cap `AGENT_RUNTIME_MEMORY_RERANKER_MAX_CANDIDATES` (default 300,
range 1–10000). The fused cap applies before both neural reranking and RRF
combined scoring, matching the pinned reference's order of operations. This matters
because the passthrough relevance score is normalized across that capped pool.

For a retrieval-only correction, `--reuse-retained-from OLD_REPORT` on the
local pipeline can reuse compatible persistent bank IDs and stored histories.
Copy the corresponding SQLite banks into the new work directory first. The
runner checks corpus/source pins, model/dimension/extraction configuration and
local/reference status, records the old report hash, and recomputes all answers
and judgments. This is suitable only when the retain/extraction implementation
has not changed; changed extraction requires fresh banks. It never carries a
previous quality verdict across executables.


## Practical desktop profile

Cloud integration is deferred at the user's request. The local backend remains
one SQLite database and calls the configured model endpoint; no Python or
PostgreSQL process is required by the desktop app.

Retrieval supports ISO dates, today/yesterday/tomorrow, this/last/next
week/month/year, rolling numeric periods and named months with a year.
SQLite FTS5 scoring and literal normalized entity matching are deliberate
storage differences; this implementation does not reproduce PostgreSQL's rank
functions or fuzzy entity registry. Use separate host-selected banks for source
isolation rather than an additional tag-rule system.

Consolidation skips whitespace-equivalent duplicate creates with case preserved.
`ConsolidateMemory` halves its batch on invalid model output or capacity overflow;
transport failures and refusals remain errors, and uncommitted facts stay eligible
for retry. `SQLiteConfig.MaxObservations` optionally caps each bank (zero means
unlimited); the provider receives its remaining capacity and writes enforce it.

Mental-model search returns its best hit in full and other hits as snippets.
Reflect must read a snippet's model before citing it. Refreshed model embeddings
include the generated content as well as the name and source query.

The practical comparison uses six complete-history LongMemEval questions, one
per category, plus paired preference, knowledge-update and abstention feature
cases. It is distinct from the existing 500-question acceptance gate and from
exhaustive equivalence with every Hindsight configuration.
