**Edit-tool plan, final: five bounded correctness fixes. No edit-format change.**

Date: 2026-09-12. Follows the [minimal runtime plan](2026-09-12-minimal-runtime-plan.md). Scope is the existing native workspace tools. Each item is a defect or inconsistency verified in the current code, fixable in one function with a focused test. Tolerant matching, failure diagnostics, hashline, counters, and baseline runs are out of scope; the earlier revisions of this document are superseded.

| # | Fix | Code | Test |
| --- | --- | --- | --- |
| 1 | Reject missing mutation fields | [write_file](/root/agent-runtime/internal/tools/workspace_tools.go:410), [edit_file](/root/agent-runtime/internal/tools/workspace_tools.go:448) | missing vs explicit empty |
| 2 | Same read budget for `read_symbol` as `read_files` | [read_symbol](/root/agent-runtime/internal/tools/workspace_symbol_tools.go:296) | budget with managed context on and off |
| 3 | Return candidates for ambiguous symbols | [read_symbol](/root/agent-runtime/internal/tools/workspace_symbol_tools.go:277) | two same-named symbols in one file |
| 4 | Bounded before/after diff in edit results | [edit_file result](/root/agent-runtime/internal/tools/workspace_tools.go:515) | replacement, deletion, cap |
| 5 | Patch line-ending and BOM handling | [applyPatchHunks](/root/agent-runtime/internal/tools/workspace_patch_tools.go:302) | CRLF and BOM preservation |

**1. Validate missing mutation fields.** `write_file` unmarshals into a plain string, so a call without `content` writes an empty file. `edit_file` only checks that `old_string` is non-empty, so a call without `new_string` deletes the match. Decode both fields as pointers, reject null or absent with an error naming the field, and keep explicit empty strings working: empty `content` still creates an empty file, empty `new_string` still deletes. Apply the same rule to `apply_patch`'s `patch` field for consistency. Tests: absent, null, and empty for each field.

**2. Give `read_symbol` the same effective budget as `read_files`.** `read_symbol` passes the fixed 2,100-rune constant to the window reader. `read_files` selects 8,192 when managed context is on. Reuse the same selection through the existing `nativeManagedReadBudget` helper so a symbol read has the same allowance as a range read of the same lines. Tests: symbol longer than 2,100 runes read in one call with managed context, clamped without.

**3. Return candidates for ambiguous symbols consistently.** `read_symbol` takes the first match and appends a warning when several symbols share a name. Return an error listing each candidate with its kind, container, and line range, and instruct the model to re-call with the qualifying container or use `read_files` on the chosen range. This matches the existing `list_symbols` output shape and the unique-match rule already enforced by `edit_file` and `apply_patch`. Tests: two classes defining `save` in one file, and a single match unchanged.

**4. Show a compact before/after edit diff.** `edit_file` returns the line number and the new text only. Produce a unified-style excerpt of the changed region with removed lines prefixed `-` and added lines `+`, three lines of context, capped at 60 lines and 3,000 characters with a truncation marker. Deletions then show what was removed. Reuse the line location already computed. Keep the first line of the result unchanged for existing consumers. Tests: a replacement, a pure deletion, a multi-line insertion, and a change exceeding the cap.

**5. Make patch line-ending handling consistent with exact edits.** `apply_patch` normalizes the patch text to LF but splits the file on `\n` only, so every line of a CRLF file keeps its `\r` and no hunk can match; a leading BOM has the same effect on the first line. Mirror `edit_file`: detect a uniformly CRLF file and a BOM, match and apply hunks against normalized content, then write back with the original line endings and BOM restored. Mixed line endings are left untouched and matched exactly, as today. Tests: LF patch against a CRLF file preserving endings, a BOM file preserving the BOM, a mixed-endings file unchanged in behavior, and an LF file unchanged in bytes outside the hunk.

**Outside this list.** Enabling `native_context` on the coding presets is a Helpin configuration choice, not a tool change, and is not part of this plan.

**Sequencing.** One runtime patch containing all five, after the P0 workspace fallback fix. No model runs required; every item is covered by unit tests on the tool functions.
