# Agent Ownership and Execution Boundary

Agent Runtime has one reusable `Agent` definition and one durable `AgentRun`
execution primitive. It does not have separate system-agent and custom-agent
executors.

Terms such as **system agent**, **custom agent**, **workspace agent**, and
**preset** belong to the host application's product and configuration model.
They may be sent to the runtime as ordinary prompt, skill, tool, target, and
execution configuration, but they do not select a different engine path.

## Responsibility boundary

| Concern | Host application | Agent Runtime |
| --- | --- | --- |
| Decide which agents exist | Yes | No |
| System/custom ownership semantics | Yes | No |
| Workspace and team tenancy | Yes | No; runtime records are partitioned by `app_id` |
| Presets, templates, and configuration versions | Yes | Stores only the executable definition it receives |
| User authorization and entitlements | Yes | Enforces runtime and tool policy on accepted runs |
| Trigger and product launch surfaces | Yes | Executes a run after the host starts it |
| Target context and domain commands | Supplies adapters/providers | Resolves and invokes them through generic contracts |
| Runtime adapter selection | Sets `runtime_kind` | Executes `native_sdk`, `codex`, or `opencode` |
| Durable workflow, transcript, tools, interactions, artifacts, usage | Consumes/projects events | Owns execution and emits records/events |

## Registration and launch

Agent Runtime never invents or seeds product agents. A host registers an
app-scoped executable definition through `PUT /v1/agents/{agent_id}` and starts
a run through `POST /v1/runs`.

The normal host flow is:

```text
host-owned agent configuration
  -> authorize user, target, and launch
  -> upsert executable Agent definition
  -> create AgentRun for a generic TargetRef
  -> select adapter from runtime_kind
  -> resolve target, skills, tools, and optional workspace
  -> execute
  -> emit messages, interactions, artifacts, status, and usage
  -> host projects results into its product records
```

All agent ownership styles use this flow. A run's `allowed_tools` may narrow,
but cannot expand, the registered agent's tool policy.

## Configuration, not agent classes

Behavior should be composed from:

- system prompt and instruction preamble;
- skill references and resolved skill policy;
- allowed tools and targets;
- provider, model, and `runtime_kind`;
- invocation, approval, completion, and workspace policy;
- target context and host-provided domain tools.

Do not add runtime branches based on a host label such as `is_system` or
`is_custom`. If a product persona needs special behavior, first represent it as
a prompt, skill, tool, target, or execution-policy contract. Host-owned
orchestration may wrap a run for genuine product workflows, but the run itself
still enters the generic executor.

## Workspace terminology

An execution workspace (`WorkspaceLease`) is a filesystem or repository lease
used while a run executes. It is not the same thing as a host application's
customer workspace or tenant.

The runtime uses `app_id` as its durable partition. Host workspace IDs may
appear in target metadata or provider calls, but their authorization and
lifecycle remain the host's responsibility.

## Helpin mapping

In Helpin:

- system agents are product-managed, usually preset-backed agent records;
- custom agents are workspace-managed, versioned agent records;
- both create the same Helpin `agent_run` and are projected into this runtime;
- Atlas, Scribe, Forge, Lens, Echo, and other named personas are configuration
  bundles and product surfaces, not Agent Runtime subclasses.

The canonical Helpin-side reference is
`helpin/docs/AGENTS_AND_AUTOMATION.md`.
