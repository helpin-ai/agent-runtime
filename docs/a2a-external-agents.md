# Run external agents over A2A

Delegate a run to an agent that lives on another service, such as a hosted
coding or research agent, through the [A2A protocol](https://a2a-protocol.org)
v1.0, or v0.3 for older agents. The external agent does the work with its own model and tools. Agent
Runtime sends it the run's message, follows the remote task, and reports what
the agent says back to your application as ordinary run messages and events.

For example: your application assigns a task, "Record a demo of the new login
flow", to an external agent. The agent asks which environment to use, a person
replies, and the agent finishes with a short summary and a link to the video.
Your application decides where the question, the answer, and the file appear.

## Register the agent

Create an agent with `runtime_kind: "a2a"`. It needs no provider, model, system
prompt, tools, or skills, and runs never receive model credentials or a
workspace. Keep your own reference in `execution_config`:

```json
{
  "app_id": "app-a",
  "name": "Hermes",
  "runtime_kind": "a2a",
  "allowed_targets": ["task"],
  "execution_config": { "external_a2a_agent_id": "ext-123" }
}
```

Runs start, pause, resume, and cancel through the usual run API. Durable runs
use their own Temporal task queue, `agent-a2a` (with any
`TEMPORAL_TASK_QUEUE_PREFIX`). Shared and all-role workers poll it with 32
concurrent turns per process, because a turn mostly waits on the remote agent.

## Supply the connection in the target context

Agent Runtime stores no endpoint or credential for the external agent. At the
start of every turn, your [target context endpoint](interfaces.md) returns the
connection under `data.a2a`:

```json
{
  "data": {
    "a2a": {
      "external_agent_id": "ext-123",
      "name": "Hermes",
      "card_url": "https://agent.example.com/.well-known/agent-card.json",
      "agent_card": { "...": "optional cached A2A AgentCard (v1.0 or v0.3)" },
      "auth": { "type": "bearer", "token": "example-token" },
      "context_id": "remote context from an earlier run, or empty",
      "message_appendix": "Text appended to the first message of each run",
      "allow_private_network": false,
      "max_turn_seconds": 6600
    }
  }
}
```

The adapter reads `data.a2a` and removes it from the target context before
anything else runs. The token is sent only as `Authorization: Bearer` to the
agent. It is never written to run metadata, messages, events, logs, or errors.
Return `data.a2a` only for active runs of agents that your application still
allows. Keep returning it while a run is being cancelled: Agent Runtime asks for
the connection again to cancel the remote task, before it marks the run
cancelled. If your application marks its own run terminal first and stops
returning `data.a2a`, the remote task is not cancelled.

The adapter uses the cached card when it lists an interface, and otherwise
fetches `card_url`. It prefers the JSON-RPC binding, then HTTP+JSON, and sends
`A2A-Version: 1.0`. Unless `allow_private_network` is true, it requires HTTPS and
refuses to connect to loopback, private, link-local, multicast, or unspecified
addresses. Responses larger than 8 MiB are rejected.

## How a turn runs

1. The first turn sends the run instructions, the target context summary, and
   `message_appendix`, with message ID `helpin-<run_id>-initial`. A later turn
   sends the resume content with message ID `helpin-<resume_id>`.
2. Every message carries the known `contextId`, from the run or from
   `data.a2a.context_id`. A reply to a task that asked for input also carries its
   `taskId`, so the agent continues the same task.
3. If the agent card sets `capabilities.streaming`, the message is sent with
   `SendStreamingMessage`. The task ID is recorded from the first event, so a
   cancellation reaches the task while it works, and the stream is read to the
   end: some agents fail a task whose stream disconnects. Otherwise the message
   is sent with `returnImmediately`. Either way, if the task is not finished when
   the send returns or the stream drops, the adapter polls `GetTask` every 1 to
   10 seconds.
4. The turn ends when the task leaves the working states:

| Remote state | Run result |
| --- | --- |
| `completed` | Assistant message with the answer, or `<name> finished without a reply`; the turn finishes |
| `input_required`, `auth_required` | Assistant message with the question; the run pauses for input |
| `failed`, `rejected` | Run fails with `External agent <name> <state>: <message>` |
| `canceled` | Run fails with `External agent <name> canceled the task`, unless the run itself is being cancelled |

An agent that answers with a message instead of a task completes the turn with
that message.

## Handle the `a2a.task` event

The run emits `a2a.task` on every observed state change and once when the turn
ends. The final event carries the answer and any files:

```json
{
  "external_agent_id": "ext-123",
  "context_id": "ctx-1",
  "task_id": "task-1",
  "state": "completed",
  "message": "Demo recorded.",
  "files": [{ "name": "demo.mp4", "media_type": "video/mp4", "url": "https://files.example.com/demo.mp4" }],
  "message_id": "helpin-run_123-initial"
}
```

`files` lists only HTTP(S) URL parts. Inline file bytes are not forwarded. Fetch
each URL with your own size and type limits before storing it.

## Retries, cancellation, and limits

- **Retries.** After a successful send the adapter records
  `{context_id, task_id, last_message_id, state}` in `run.input.metadata.a2a`. A
  retried turn with the same message ID follows the recorded task instead of
  sending the message again. If an agent answered with a message and created no
  task, a retry sends the message again with the same message ID.
- **Cancellation.** Cancelling a run asks the agent to cancel its task, with a
  10-second limit, before the run becomes terminal. Agent Runtime resolves the
  target context again to get the connection. The agent's `canceled` answer can
  reach the worker first; it waits up to 10 seconds for the run to be marked
  cancelled, so the run ends cancelled, not failed. A worker shutdown drains the
  turn for up to the worker stop timeout and does not cancel the remote task; a
  retried turn follows it. An agent that fails tasks whose stream disconnects
  (Hermes) fails a task cut off by a hard worker stop.
- **Turn limit.** A turn may last `max_turn_seconds`, 110 minutes by default,
  counted from the start of the turn. The execution activity allows two hours.
  When the limit passes, the adapter asks the agent to cancel the task and the
  run fails with `External agent <name> did not finish within 110 minutes`.
- **Content.** Only text parts and URL file parts are read. Data parts, inline
  bytes, and push notifications are not used. Streamed status and artifact
  updates are applied; where the stored task has no reply, the streamed one is
  kept.

## Agent notes

- **Hermes** (`a2a` gateway platform). Advertises streaming, so cancellation
  works mid-task. Its plain `SendMessage` ignores `returnImmediately` and stays
  open until the task ends. A reply starting with `[INPUT_REQUIRED]` becomes
  `input_required`. Hermes runs tasks in one live session: a second task sent
  while one is working interrupts it, and the interrupted turn ends without a
  reply, so send one task at a time. It rejects a context after
  `A2A_MAX_PINGPONG_TURNS` turns (default 5, at most 20) and fails a task after
  `A2A_REPLY_TIMEOUT` seconds (default 300). It returns text only; it can attach
  files with the upload command in `message_appendix`.
