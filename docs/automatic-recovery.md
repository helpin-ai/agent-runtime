# Automatic connection recovery

Set `AGENT_RUNTIME_AUTOMATIC_RECOVERY=true` for a single-owner native lightweight
runtime to keep runs pending through temporary provider or network failures.
Rex enables this together with `AGENT_RUNTIME_RECOVER_INTERRUPTED_RUNS=true`.
Other deployments retain their existing failure behavior by default.

The existing lightweight execution slot owns both execution and its retry wait.
The run remains `running`; `input.metadata.runtime_recovery` contains its reason,
transport attempt count, and UTC `next_retry_at`. Existing SDKs and store schemas
preserve this metadata, so no SDK release or database migration is required.
A conditional store write prevents a failed provider request from overwriting a
concurrent cancellation. Completed native tool checkpoints and turn identities
are retained. Work-job workers keep their attempts and accepted item receipts.

Retries start after five seconds, double up to thirty seconds, and continue
through extended outages. Supported provider errors preserve a longer
`Retry-After` deadline. On wake, an overdue timer resumes execution; after process
restart the existing ordinary-run and job recovery owners restore pending waits.
Provider authentication, quota/billing failures, invalid requests, checkpoint
failures, and ambiguous tool outcomes retain their existing error/interaction
handling. This policy does not automatically replay failed tool calls.

Persisted `run.recovery_wait` and `run.recovery_started` events let clients show
waiting without marking the turn failed. Rex preserves completed steps, shows
waiting workers, and excludes recorded retry waits from active work duration.
Rex's scheduler atomically claims the latest overdue occurrence and advances its
schedule; multiple missed occurrences coalesce. Open occurrences are reconciled
before another occurrence starts, and temporary lookup failures preserve claims.

The implementation uses timer-based recovery, with ordinary retry eligibility
within thirty seconds of network restoration, subject to provider deadlines and
execution capacity. Native OS reconnect hooks and broader provider-wide cooldown
scheduling from the original design remain future work. Historical terminal runs
are not automatically revived. A disabled routine prevents future occurrences;
its already admitted run follows the existing run cancellation controls.
