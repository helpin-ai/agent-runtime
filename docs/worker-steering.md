# Worker recovery and steering

## Reconnect the existing job

`GET /v1/work-jobs/{id}/credentials?app_id=...` returns attached connection IDs and an opaque credential version, never secrets. `PUT` at the same path takes `expected_version` and replacements for existing MCP attachments or the existing model identity. A stale version returns a conflict. Rotation cannot add destinations, accounts, or tools. Worker templates remain encrypted at rest.

Hosts refresh the template first, rotate each blocked worker's credential, then resolve that worker's authentication interaction with a stable resume ID. This preserves completed items and fixes subsequent batches as well as the blocked one. A parent job pause prevents worker resumption transactionally. Updating credentials alone never resumes a paused job. Connection refresh failures remain authentication blockers, not successful item outcomes.

Rex watches hidden workers as well as conversations. A new OAuth sign-in wakes the watcher. MCP and model blockers name their actual connection; neither is inferred from a generic `worker_auth` job state. A refreshed MCP token rejected again waits for a new sign-in instead of creating a refresh loop. The recovery record stores only a fingerprint and interaction ID. An explicit job pause remains in force.

## Steer a run

`POST /v1/runs/{id}/steering?app_id=...` accepts:

```json
{
  "command_id": "host-generated-stable-id",
  "expected_revision": 0,
  "external_actor_id": "trusted-host-actor",
  "content": "Research only; preserve source URLs.",
  "denied_tools": ["mcp__crm__update_company"]
}
```

The response is a durable receipt. Read receipts with `GET` at the same path. Command IDs deduplicate identical requests; changed payloads or stale revisions conflict. Commands do not resume paused runs or resurrect terminal runs.

- `accepted`: persisted, waiting for a safe execution boundary.
- `applied`: the native instruction is checkpointed, or the remote agent explicitly acknowledged it.
- `rejected`: the remote agent explicitly refused the instruction.
- `not_applied`: the run ended before application.

For native execution, the boundary is between model steps, including a check after a response attempts to finish. A checkpoint written before its acknowledgement can be recovered without adding the instruction twice. Temporal workers execute the same boundary logic within their runtime activity.

Tool restrictions are cumulative and enforced before native tool admission, independently of the prompt. Name the exact canonical tools to prohibit. This does not roll back or cancel a tool call already admitted. Check its result before repeating an external write. Text instructions alone do not guarantee that every possible write path is disabled.

## Steer a job

Revise the existing job with the current `expected_revision` and a `spec.steering` object containing `content` and optional `denied_tools`. The persisted specification drives restart-safe delivery to active and future workers. Each receipt uses a stable job/specification command ID. Read active worker receipts before reporting that the instruction applied.

An instruction-only revision keeps active batches; their next results are accepted only after the new instruction is acknowledged. A result-schema or other specification change retains the existing revision fence and retries unresolved items under the new specification. Accepted results retain their original revision and are not overwritten. Restrictions cannot be removed by a later specification update. Worker output already accepted before the revision stays accepted.

Rex exposes this through `manage_batch_job` and **Steer workers**. Individual worker steering is also available through the tool. Helpin's dock composer sends steering to the existing active native run, with workspace and chat visibility checks and trusted actor attribution.

## A2A extension v1

Steering uses standard A2A messages and structured data parts with the declared extension URI:

`https://helpin.ai/a2a/extensions/steering/v1`

This is a Helpin extension, not a core A2A pause operation. A remote agent must advertise it in `AgentCard.capabilities.extensions`; the runtime activates it using the A2A extension service parameter. Unsupported or not-yet-discovered agents reject steering explicitly.

The message retains the existing task and context IDs, uses a stable message ID, and includes the extension URI in `extensions`. Its data part contains `{ "extension": "<URI>", "command": <RunSteeringCommand> }`. The receiver must deduplicate command IDs, enforce any declared restrictions at its own safe boundary, and acknowledge with a data part:

```json
{
  "extension": "https://helpin.ai/a2a/extensions/steering/v1",
  "command_id": "host-generated-stable-id",
  "revision": 1,
  "state": "applied"
}
```

A receipt must match command ID, revision, task ID, and context ID. `rejected` is also supported. Prose, successful HTTP delivery, or task progress is never treated as application. Unacknowledged delivery can be retried with the same ID, bounded by the session polling interval. Delivery errors leave the command pending and do not terminate otherwise productive remote work. Streaming sessions deliver at task-event boundaries; quiet streams may defer delivery until the next event.

## Deployment and rollback

Release the SDK first, then runtime, then consuming hosts. The PostgreSQL startup migration and SQLite migration add the steering table; no existing item or credential data is rewritten. Native execution requires the new table before serving traffic. Run the migration and upgrade all execution workers before exposing steering controls: an older worker cannot enforce a new command. Rolling back execution workers while steering commands are active is unsafe; first pause/drain those runs. Hosts can roll back independently without deleting job state.

Validation covers encrypted credential CAS, tenant boundaries, parent-pause preservation, restart deduplication, concurrent revision conflicts on SQLite and PostgreSQL, late model-response instructions, exact A2A receipts, and 851 synthetic items with three workers. No live CRM mutations or customer research are required for these checks.
