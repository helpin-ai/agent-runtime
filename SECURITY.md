# Report a security issue

Keep service tokens, provider credentials, private prompts, and customer records
out of public issues, pull requests, and discussions.

## Private reporting

Use **Security → Report a vulnerability** in the
[Agent Runtime repository](https://github.com/helpin-ai/agent-runtime/security)
when that option is enabled. The
[private report form](https://github.com/helpin-ai/agent-runtime/security/advisories/new)
belongs to this repository.

If the option is unavailable, do not post vulnerability details in an issue.
Ask a repository maintainer to enable private reporting, without including the
sensitive details. This policy does not itself enable GitHub’s reporting feature.

Include the affected runtime and SDK versions, the execution mode, a reproduction
using synthetic data, and the expected versus observed boundary. Do not include
live credentials or access other users’ records to demonstrate an issue.

## Before deploying

Keep the runtime service API behind your backend and use a service token. Enforce
record authorization in the host application, limit exposed tools, and review the
operating requirements of your worker configuration. See
[runtime configuration](docs/runtime-configuration.md) and
[host integration](docs/app-configuration.md).
