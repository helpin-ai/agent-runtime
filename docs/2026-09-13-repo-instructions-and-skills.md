**Repository instructions and repository skills for native runs.**

Date: 2026-09-13. Two bounded additions to the native runtime. Both read only from the run's repository checkout; nothing on the worker outside the checkout is consulted, because worker pods are ephemeral. No new tools, no new tables, no Helpin changes.

| # | Change | Where | Gate |
| --- | --- | --- | --- |
| 1 | Inject the checkout's top-level instruction file into the system prompt | `internal/runtime/native_exec.go` | prompt tests with and without a file, override precedence, cap |
| 2 | Discover `.agents/skills/**/SKILL.md` in the checkout and expose them through the existing skill catalog | `internal/engine/engine.go` staging, `internal/skills/loader.go` | staging tests, catalog and read_skill tests, limits |

**Current state.** The coding prompt tells the model to "read applicable AGENTS.md instructions" and nothing loads the file ([native_exec.go](/root/agent-runtime/internal/runtime/native_exec.go:778)). The system prompt is rebuilt from `ExecutionContext` at the start of every execution ([native_exec.go](/root/agent-runtime/internal/runtime/native_exec.go:226)) and is not part of the compacted message history, so anything placed there survives compaction without anchors. There is already a workspace section in that prompt built from the lease metadata ([native_exec.go](/root/agent-runtime/internal/runtime/native_exec.go:813)). Skills reach the model through `find_skills` and `read_skill`, backed by a staged manifest under `<checkout>/.agent-runtime/skills` ([engine.go](/root/agent-runtime/internal/engine/engine.go:1040)); `read_skill` only resolves paths under that staged root. The SKILL.md parser accepts any `fs.FS` and a source kind ([loader.go](/root/agent-runtime/internal/skills/loader.go:60)).

**1. Repository instruction file.**

- When the execution context has a workspace lease with a root path, look in that directory only for, in order, `AGENTS.override.md`, `AGENTS.md`, `CLAUDE.md`. Take the first that exists and is a regular file. No ancestor walk, no subdirectories, no home or config directories.
- Read at most 32 KiB. If the file is larger, keep the head and append one line saying it was truncated at that size, so the model knows to read the rest with `read_files`.
- Append to the system prompt as its own section after the workspace section: a header naming the file path relative to the checkout, then the content verbatim. Strip a BOM. Treat the content as repository-provided instructions, not as host instructions; the header says so.
- Apply to every run with a repository lease, not only coding profiles, so Ask Agent, Epic Planner, and Documentation Agent reading a repository get the same context. Keep the existing "read applicable AGENTS.md" sentence for the case where the model enters a subdirectory that has its own file.
- Log which file was injected, with the run id, at the start of each execution for provenance. The prompt section itself names the file.
- Failure to read the file is logged and skipped, never fatal.

Tests: no lease; lease without any file; each filename alone; override winning over the plain file; a file over the cap truncated with the marker; a BOM file; a directory named `AGENTS.md` ignored; the section absent from public messages and present in every rebuilt prompt across a resume.

**2. Repository skills.**

- During skill staging, after the lease exists, scan `<checkout>/.agents/skills`. A skill is a directory containing `SKILL.md`; do not recurse below a skill directory. Ignore symlinks. Stop after 32 skills and record that the limit was hit.
- Parse each with the existing package loader over `os.DirFS(checkout)` and source kind `repository`. Skip a skill whose front matter has no name or description, and skip a key that collides with a built-in or workspace skill; the host-owned skill wins and the collision is recorded.
- Copy each accepted skill directory into the staged root under `repository/<key>`, capped at 1 MiB per skill and excluding symlinks, so `read_skill` works unchanged and its path guard still applies. The checkout itself is not read through `read_skill`.
- Add the entries to the staged manifest with source kind `repository` so `find_skills` lists them alongside the others. They are available skills only. They never become active skills, carry no policy, and cannot require tools; the activation policy layer stays host-owned.
- Skills from the repository are repository-provided instructions with the same trust as item 1. They apply only to runs already checked out on that repository. Nothing in a skill directory is executed by staging.

Tests: a checkout with two skills discovered and listed; a nested directory below a skill not treated as a second skill; a skill without front matter skipped; a key collision resolved in favor of the workspace skill; the count and size caps; a symlink inside a skill directory excluded; `read_skill` reading a repository skill and a referenced relative file; a run without a lease unchanged.

**Not doing.** Ancestor or nested instruction files. User-level, admin-level, or machine-level skills. Claude skill directories. Codex UI metadata files. Implicit skill activation from repository skills. Executing repository skill scripts during staging.

**Sequencing.** Item 1 first as one runtime commit; it removes a read call from every coding run and fixes the case where the model skips the file. Item 2 as a second commit when a repository we work on actually ships skills. Both after the current PR lands, since they touch prompt assembly and staging that the PR already changes.
