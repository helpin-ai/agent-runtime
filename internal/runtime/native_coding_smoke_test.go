package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/procenv"
)

// TestNativeCodingSmoke is an opt-in, API-key-backed release gate. All model
// writes and commands are confined to disposable fixture repositories.
func TestNativeCodingSmoke(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_NATIVE_SMOKE") != "1" {
		t.Skip("set AGENT_RUNTIME_NATIVE_SMOKE=1 with an OpenAI API key")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OPENAI_API_KEY is required")
	}
	model := firstNonEmpty(os.Getenv("NATIVE_SMOKE_MODEL"), defaultNativeOpenAIModel)
	for _, tt := range []struct {
		name, prompt, source, extra, test string
		review                            bool
	}{
		{"patch", "Fix add() so it adds two numbers. Read repository instructions, change the implementation, inspect the diff, and run the unittest suite.", "def add(a,b):\n    return a-b\n", "", "from calc import add\nimport unittest\nclass Tests(unittest.TestCase):\n    def test_add(self): self.assertEqual(add(2,3),5)\n", false},
		{"multi_file", "Implement clamp(value, low, high) in calc.py and update display() in formatter.py to return the clamped value as a string. Read the tests and validate the multi-file change.", "def clamp(value, low, high):\n    raise NotImplementedError\n", "def display(value):\n    return 'unimplemented'\n", "from calc import clamp\nfrom formatter import display\nimport unittest\nclass Tests(unittest.TestCase):\n    def test_clamp(self): self.assertEqual(clamp(15,0,10),10)\n    def test_display(self): self.assertEqual(display(-2),'0')\n", false},
		{"review", "Review calc.py for correctness. percent is an integer percentage from 0 to 100; for example discount(100,20) must return 80. Report a concrete bug with its file and line and an example. Do not change files.", "def discount(price, percent):\n    return price * (1 - percent)\n", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			dir := t.TempDir()
			files := map[string]string{"AGENTS.md": "Use Python standard-library unittest. Keep changes focused. Do not install packages.\n", "calc.py": tt.source}
			if tt.extra != "" {
				files["formatter.py"] = tt.extra
			}
			if tt.test != "" {
				files["test_calc.py"] = tt.test
			}
			for name, content := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Smoke", "-c", "user.email=smoke@example.invalid", "commit", "-qm", "fixture"}} {
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Dir = dir
				cmd.Env = procenv.Command()
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("fixture git: %s %v", out, err)
				}
			}
			x := contextTestExec(t)
			x.Context = ctx
			x.Agent.ExecutionConfig = nil
			x.Agent.Provider = "openai"
			x.Agent.Model = model
			x.Agent.ApprovalMode = agentcore.ApprovalModeNever
			x.AllowedTools = map[string]bool{"read_files": true, "list_directory": true, "repository_search": true, "run_command": true}
			if !tt.review {
				x.AllowedTools["edit_file"] = true
				x.AllowedTools["apply_patch"] = true
				x.AllowedTools["write_file"] = true
			}
			x.Agent.AllowedTools = nil
			for name := range x.AllowedTools {
				x.Agent.AllowedTools = append(x.Agent.AllowedTools, name)
			}
			x.Run.Input.Instructions = tt.prompt
			x.WorkspaceLease = &agentcore.WorkspaceLease{RootPath: dir}
			x.Run.WorkspaceLease = x.WorkspaceLease
			result, err := executeNativeModel(ctx, x, NativeConfig{ModelFactory: EinoProviderFactory{OpenAIAPIKey: key}, MaxToolSteps: 20})
			if err != nil {
				t.Fatal(err)
			}
			if result.MaxSteps || result.AwaitingInput || result.AwaitingApproval {
				t.Fatal("smoke task did not finish")
			}
			if tt.review {
				content, err := os.ReadFile(filepath.Join(dir, "calc.py"))
				if err != nil {
					t.Fatal(err)
				}
				if string(content) != tt.source || !strings.Contains(result.AssistantText, "calc.py") || !strings.Contains(result.AssistantText, "100") {
					t.Fatalf("review missed the seeded percentage bug: %s", result.AssistantText)
				}
			} else {
				cmd := exec.CommandContext(ctx, "python3", "-m", "unittest")
				cmd.Dir = dir
				cmd.Env = procenv.Command()
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("fixture tests failed: %s %v", out, err)
				}
			}
		})
	}
}
