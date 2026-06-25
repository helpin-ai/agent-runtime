package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestSkillToolsListAndReadStagedFiles(t *testing.T) {
	workspace := t.TempDir()
	skillRoot := filepath.Join(workspace, ".agent-runtime", "skills", "01-demo")
	if err := os.MkdirAll(filepath.Join(skillRoot, "examples"), 0o755); err != nil {
		t.Fatalf("mkdir skill root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "SKILL.md"), []byte("Use this skill."), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "examples", "trend.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatalf("write example: %v", err)
	}

	registry := NewRegistry()
	callCtx := CallContext{Run: &agentcore.AgentRun{WorkspaceLease: &agentcore.WorkspaceLease{RootPath: workspace}}}

	listed, err := registry.Execute(context.Background(), callCtx, "skills.list_files", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list skill files: %v", err)
	}
	if !strings.Contains(string(listed), `"01-demo/SKILL.md"`) || !strings.Contains(string(listed), `"01-demo/examples/trend.json"`) {
		t.Fatalf("unexpected listing: %s", string(listed))
	}

	read, err := registry.Execute(context.Background(), callCtx, "skills.read_file", json.RawMessage(`{"path":"01-demo/SKILL.md"}`))
	if err != nil {
		t.Fatalf("read skill file: %v", err)
	}
	if !strings.Contains(string(read), `"content":"Use this skill."`) {
		t.Fatalf("unexpected read output: %s", string(read))
	}
}

func TestSkillToolsRejectPathEscapes(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".agent-runtime", "skills"), 0o755); err != nil {
		t.Fatalf("mkdir skill root: %v", err)
	}
	registry := NewRegistry()
	callCtx := CallContext{Run: &agentcore.AgentRun{WorkspaceLease: &agentcore.WorkspaceLease{RootPath: workspace}}}

	if _, err := registry.Execute(context.Background(), callCtx, "skills.read_file", json.RawMessage(`{"path":"../secret"}`)); err == nil {
		t.Fatal("expected parent path to be rejected")
	}
	if _, err := registry.Execute(context.Background(), callCtx, "skills.read_file", json.RawMessage(`{"path":"/tmp/secret"}`)); err == nil {
		t.Fatal("expected absolute path to be rejected")
	}
}
