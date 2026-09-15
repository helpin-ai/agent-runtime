package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func repoInstructionsExec(root string) *ExecutionContext {
	return &ExecutionContext{
		Agent:          &agentcore.Agent{SystemPrompt: "Plan the task."},
		WorkspaceLease: &agentcore.WorkspaceLease{Provider: "repository", RootPath: root},
	}
}

func TestRepositoryInstructionsAbsentWithoutLeaseOrFile(t *testing.T) {
	if section, file := nativeRepositoryInstructions(&ExecutionContext{Agent: &agentcore.Agent{}}); section != "" || file != "" {
		t.Fatalf("expected nothing without a lease, got %q from %q", section, file)
	}
	if section, file := nativeRepositoryInstructions(repoInstructionsExec(t.TempDir())); section != "" || file != "" {
		t.Fatalf("expected nothing without a file, got %q from %q", section, file)
	}
	if strings.Contains(nativeSystemPrompt(repoInstructionsExec(t.TempDir())), "Repository instructions") {
		t.Fatal("prompt must not contain an instructions section without a file")
	}
}

func TestRepositoryInstructionsPrecedenceAndFallbacks(t *testing.T) {
	for _, name := range nativeRepositoryInstructionFiles {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, name), []byte("Run make test before finishing.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			section, file := nativeRepositoryInstructions(repoInstructionsExec(root))
			if file != name || !strings.Contains(section, "from "+name) || !strings.Contains(section, "Run make test before finishing.") {
				t.Fatalf("unexpected section for %s: file=%q section=%q", name, file, section)
			}
			if !strings.Contains(nativeSystemPrompt(repoInstructionsExec(root)), "Run make test before finishing.") {
				t.Fatal("system prompt must include the repository instructions")
			}
		})
	}
	root := t.TempDir()
	for name, content := range map[string]string{"AGENTS.override.md": "override wins", "AGENTS.md": "plain loses", "CLAUDE.md": "claude loses"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	section, file := nativeRepositoryInstructions(repoInstructionsExec(root))
	if file != "AGENTS.override.md" || !strings.Contains(section, "override wins") || strings.Contains(section, "loses") {
		t.Fatalf("override precedence broken: file=%q section=%q", file, section)
	}
}

func TestRepositoryInstructionsCapBOMAndDirectory(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", nativeRepositoryInstructionsMaxBytes-1) + "é" + strings.Repeat("y", 100)
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("\ufeff"+big), 0o644); err != nil {
		t.Fatal(err)
	}
	section, _ := nativeRepositoryInstructions(repoInstructionsExec(root))
	if strings.Contains(section, "\ufeff") {
		t.Fatal("BOM must be stripped")
	}
	if !strings.Contains(section, "truncated at") || strings.Contains(section, "yyy") {
		t.Fatalf("large file must be truncated with a marker: %q", section[len(section)-120:])
	}
	if strings.Contains(section, "�") || !strings.HasSuffix(strings.SplitN(section, "\n[", 2)[0], "x") {
		t.Fatal("truncation must not split a multi-byte character")
	}

	dirRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(dirRoot, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirRoot, "CLAUDE.md"), []byte("fallback"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, file := nativeRepositoryInstructions(repoInstructionsExec(dirRoot)); file != "CLAUDE.md" {
		t.Fatalf("a directory named AGENTS.md must be ignored, got %q", file)
	}
}
