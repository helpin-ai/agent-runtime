package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Repository skills are staged under repository/<key> and must be listed and
// readable through the same tools as host skills.
func TestAvailableSkillToolsReadRepositorySkills(t *testing.T) {
	// Mirror the layout StageRepositorySkills produces: repository/<key> under
	// the staged root, with a manifest entry whose package_dir is nested.
	checkout := t.TempDir()
	root := filepath.Join(checkout, ".agent-runtime", "skills")
	skillDir := filepath.Join(root, "repository", "release_notes")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: release_notes\ndescription: Write release notes for this repo\n---\nSummarize merged PRs."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "template.md"), []byte("## Template"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"skills":[{"key":"release_notes","title":"release_notes","description":"Write release notes for this repo","source_kind":"repository","package_dir":"repository/release_notes"}]}`
	if err := os.WriteFile(filepath.Join(root, stagedSkillManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := NewRegistry()
	callCtx := CallContext{StagedSkillRoot: root}
	listed, err := registry.Execute(context.Background(), callCtx, "find_skills", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(listed), `"key":"release_notes"`) {
		t.Fatalf("unexpected list output %s, err=%v", listed, err)
	}
	searched, err := registry.Execute(context.Background(), callCtx, "find_skills", json.RawMessage(`{"query":"repository"}`))
	if err != nil || !strings.Contains(string(searched), `"total":1`) || !strings.Contains(string(searched), `"source_kind":"repository"`) {
		t.Fatalf("unexpected search output %s, err=%v", searched, err)
	}
	read, err := registry.Execute(context.Background(), callCtx, "read_skill", json.RawMessage(`{"key":"release_notes"}`))
	if err != nil || !strings.Contains(string(read), "Summarize merged PRs.") {
		t.Fatalf("unexpected read output %s, err=%v", read, err)
	}
	reference, err := registry.Execute(context.Background(), callCtx, "read_skill", json.RawMessage(`{"key":"release_notes","path":"references/template.md"}`))
	if err != nil || !strings.Contains(string(reference), "## Template") {
		t.Fatalf("unexpected reference output %s, err=%v", reference, err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_skill", json.RawMessage(`{"key":"release_notes","path":"../../../AGENTS.md"}`)); err == nil {
		t.Fatal("expected escape from the staged package to fail")
	}
}
