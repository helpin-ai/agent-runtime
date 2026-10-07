# Loudkeeper custom agent: repeated task approvals

Investigation date: 2026-09-26. Production queries used identifiers, tool
names, approval modes, risk labels, statuses, and counts only. No prompts,
tool arguments, outputs, or credentials were read.

## Observed behavior

The Loudkeeper workspace's custom **Dependency Auditor agent** is saved with
`approval_mode=always` in Helpin and in agent-runtime. Its completed run
`run_934a8bf5062d8d5409140b32` had 25 resolved per-tool approval requests:
22 `create_task`, one `create_document`, and two `run_command`. The 23 Helpin
tool requests carried `risk_level=routine_mutation`; the command requests
carried `sensitive_mutation`. Two earlier runs of the same agent had seven and
four command approval requests, respectively.

The same saved agent has `default_invocation_mode=autonomous` and
`trigger_mode=manual`. Invocation mode means it works independently once
started; trigger mode determines whether it starts on its own. Neither field
overrides the separate approval policy. These values are stored configuration,
not text in the agent's system prompt.

The agent's allowed tools are `create_document`, `create_task`, `fetch_url`,
`link_document_to_object`, `list_directory`, `read_files`,
`repository_search`, `run_command`, `update_plan`, and `web_search`.

## Root cause

1. Helpin marks `create_task` and `create_document` as routine mutations in
   `server/internal/commandtools/metadata.go`. Its provider catalog publishes
   that risk level in `server/internal/service/agent_runtime_mcp.go`.
   Agent-runtime's MCP registration retains it in `internal/mcp/register.go`.
   The production approval records prove it arrived intact.
2. Agent-runtime's `nativeRequiresApproval` prompts for **every mutating tool**
   under `always`, regardless of risk level. Only `risk_based` uses the routine,
   sensitive, and destructive levels to decide whether a prompt is required.
   `always` also requires approval before the run starts.
3. The TypeSafe JEV reviewer is called only under `risk_based` and only for a
   short list of local coding tools (`run_command`, `run_python`, file edits,
   branch creation, commit/push, and PR opening). It does not review Helpin's
   `create_task` or other command-backed business tools. Thus JEV did not
   cause these task prompts, and its API status would not change them.
4. The saved agent's `template_key` is
   `engineering_dependency_auditor`. Its flow template explicitly set
   `approval_mode: always`, which explains the installed policy. The old
   frontend custom-agent form defaulted to `mutating_tools`, while the server
   fell back to `always` if a create request omitted the field; neither was
   needed to explain this particular agent.

## Appropriate change

For this existing agent, `risk_based` is the targeted configuration: its
routine `create_task`, `create_document`, and `link_document_to_object` calls
would run without per-call approval; sensitive/destructive mutations would
continue to require approval. `run_command` is classified as sensitive and
may still prompt, subject to the narrow JEV local-command review. This has
not been changed in production.

Automatic starts would require a separate trigger change (for example, an
assignment or event trigger). The approval fix alone does not create one.

Changing **all** non-deletion mutations to automatic approval would discard
the existing distinction between routine internal writes and actions such as
external messages, publishing, starting other agents, shared-branch pushes,
or sensitive data access. Tool and workspace permissions still govern whether
a call can execute, but they do not answer whether the user intended that
specific side effect. Keep explicit risk metadata for bounded Helpin actions,
fail unclassified mutations toward review, and use JEV for ambiguous eligible
commands rather than as the primary authority for known Helpin tools.

Landlock confines filesystem access for processes launched by coding tools.
Helpin command-backed task writes execute through the host service, outside
that filesystem sandbox. Landlock does not itself govern their database
effects, and the current coding-node Landlock setup does not block command
network egress. It cannot justify a blanket allow policy for business tools.

## Follow-up

- Update the existing Loudkeeper agent to `risk_based` through an authorized
  live settings path. Both Helpin and agent-runtime still store `always` after
  the code release. Existing completed runs are unaffected.
- Align the server's omitted-field default for new custom agents with the
  intended frontend policy. Commit `d677993e8` in Helpin changes new custom
  agent defaults, the Agent Creator draft, and flow template policies to
  `risk_based`. The identical tree was promoted to production as commit
  `d543f8e59` and deployed as `server-v0.95.444` after staging verification.
  These defaults affect new agents and template installs; they do not rewrite
  the Loudkeeper agent's saved policy.
- Verify future approval metrics by agent mode, tool name, risk level, and
  decision reason. A similar 22-task run under `risk_based` should have zero
  `create_task` approvals; any command approvals should be evaluated separately.
