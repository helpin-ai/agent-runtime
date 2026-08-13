package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestAvailableSkillToolsListSearchAndReadOnePackage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	packageRoot := filepath.Join(root, "01-demo")
	if err := os.MkdirAll(filepath.Join(packageRoot, "references"), 0o755); err != nil {
		t.Fatalf("mkdir skill package: %v", err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "SKILL.md"), []byte("Use this skill."), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "references", "example.md"), []byte("Example reference."), 0o644); err != nil {
		t.Fatalf("write reference: %v", err)
	}
	manifest := `{"skills":[{"key":"demo","skill_id":"skill-1","title":"Demo Skill","description":"Specialized planning","source_kind":"workspace","required_tools":["read_files"],"supported_runtimes":["native_sdk","codex"],"package_dir":"01-demo"}]}`
	if err := os.WriteFile(filepath.Join(root, stagedSkillManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	registry := NewRegistry()
	for _, oldName := range []string{"list_available_skills", "search_available_skills"} {
		if _, ok := registry.Definition(oldName); ok {
			t.Fatalf("legacy %s must not be registered", oldName)
		}
	}
	callCtx := CallContext{StagedSkillRoot: root}
	listed, err := registry.Execute(context.Background(), callCtx, "find_skills", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(listed), `"key":"demo"`) || strings.Contains(string(listed), "Specialized planning") || strings.Contains(string(listed), "Use this skill") {
		t.Fatalf("unexpected list output %s, err=%v", listed, err)
	}
	searched, err := registry.Execute(context.Background(), callCtx, "find_skills", json.RawMessage(`{"query":"planning"}`))
	if err != nil || !strings.Contains(string(searched), `"total":1`) {
		t.Fatalf("unexpected search output %s, err=%v", searched, err)
	}
	read, err := registry.Execute(context.Background(), callCtx, "read_skill", json.RawMessage(`{"skill_id":"skill-1","path":"references/example.md"}`))
	if err != nil || !strings.Contains(string(read), "Example reference.") {
		t.Fatalf("unexpected read output %s, err=%v", read, err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_skill", json.RawMessage(`{"key":"demo","path":"../secret"}`)); err == nil {
		t.Fatal("expected package path escape to fail")
	}
}

func TestAvailableSkillListReturnsCompleteCompactCatalog(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir skill root: %v", err)
	}
	type manifestSkill struct {
		Key         string `json:"key"`
		Title       string `json:"title"`
		Description string `json:"description"`
		PackageDir  string `json:"package_dir"`
	}
	manifest := struct {
		Skills []manifestSkill `json:"skills"`
	}{Skills: make([]manifestSkill, 60)}
	for i := range manifest.Skills {
		manifest.Skills[i] = manifestSkill{
			Key:         fmt.Sprintf("skill-%02d", i),
			Title:       fmt.Sprintf("Skill %02d", i),
			Description: strings.Repeat("verbose discovery metadata ", 40),
			PackageDir:  fmt.Sprintf("%02d-skill", i),
		}
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, stagedSkillManifestName), payload, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	registry := NewRegistry()
	listed, err := registry.Execute(context.Background(), CallContext{StagedSkillRoot: root}, "find_skills", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list available skills: %v", err)
	}
	output := string(listed)
	for _, key := range []string{"skill-00", "skill-29", "skill-59"} {
		if !strings.Contains(output, `"key":"`+key+`"`) {
			t.Fatalf("complete catalog is missing %q: %s", key, output)
		}
	}
	if strings.Contains(output, "verbose discovery metadata") {
		t.Fatalf("compact catalog leaked verbose descriptions: %s", output)
	}
	if len([]rune(output)) > 8000 {
		t.Fatalf("compact catalog should fit the native model-visible tool budget, got %d runes", len([]rune(output)))
	}
}
