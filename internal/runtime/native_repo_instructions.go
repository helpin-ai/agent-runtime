package runtime

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Repository instruction files are read from the checkout's top-level
// directory only. Worker pods are ephemeral, so nothing outside the checkout
// is a meaningful source of instructions.
const nativeRepositoryInstructionsMaxBytes = 32 * 1024

var nativeRepositoryInstructionFiles = []string{"AGENTS.override.md", "AGENTS.md", "CLAUDE.md"}

// nativeRepositoryInstructions returns the system-prompt section for the
// checkout's instruction file and the file name it came from. It returns
// empty values when there is no lease, no file, or the file cannot be read;
// a missing file is never an error for the run.
func nativeRepositoryInstructions(execCtx *ExecutionContext) (section, file string) {
	if execCtx == nil || execCtx.WorkspaceLease == nil {
		return "", ""
	}
	root := strings.TrimSpace(execCtx.WorkspaceLease.RootPath)
	if root == "" {
		return "", ""
	}
	for _, name := range nativeRepositoryInstructionFiles {
		path := filepath.Join(root, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		payload = bytes.TrimPrefix(payload, []byte("\ufeff"))
		truncated := false
		if len(payload) > nativeRepositoryInstructionsMaxBytes {
			payload = payload[:nativeRepositoryInstructionsMaxBytes]
			// Do not cut a multi-byte character in half.
			for i := 0; i < utf8.UTFMax && len(payload) > 0; i++ {
				if r, size := utf8.DecodeLastRune(payload); r == utf8.RuneError && size == 1 {
					payload = payload[:len(payload)-1]
					continue
				}
				break
			}
			truncated = true
		}
		content := strings.TrimSpace(string(payload))
		if content == "" {
			continue
		}
		var out strings.Builder
		fmt.Fprintf(&out, "Repository instructions from %s (provided by the repository, not by the host):\n%s", name, content)
		if truncated {
			fmt.Fprintf(&out, "\n[%s truncated at %d bytes; use read_files to read the rest]", name, nativeRepositoryInstructionsMaxBytes)
		}
		if execCtx.Run != nil {
			slog.InfoContext(execCtx.Context, "repository instructions injected", "run_id", execCtx.Run.ID, "file", name, "truncated", truncated)
		}
		return out.String(), name
	}
	return "", ""
}
