package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/skills"
)

func TestStageRuntimeSkillsExposesRepositorySkillsWithoutHostSkills(t *testing.T) {
	checkout := t.TempDir()
	skillDir := filepath.Join(checkout, ".agents", "skills", "release-notes")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: release_notes\ndescription: Write release notes\n---\nSummarize merged PRs."), 0o644); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{ID: "run-1", AppID: "app"}
	lease := &agentcore.WorkspaceLease{Provider: "repository", RootPath: checkout}

	root, _, err := (&Engine{}).stageRuntimeSkills(t.Context(), &agentcore.Agent{}, run, skills.Resolution{}, lease, nil)
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(checkout, ".agent-runtime", "skills") {
		t.Fatalf("unexpected staged root %q", root)
	}
	manifest, err := os.ReadFile(filepath.Join(root, ".available-skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"key":"release_notes"`) || !strings.Contains(string(manifest), `"source_kind":"repository"`) {
		t.Fatalf("manifest missing repository skill: %s", manifest)
	}
	if _, err := os.Stat(filepath.Join(root, "repository", "release_notes", "SKILL.md")); err != nil {
		t.Fatal("repository skill was not copied into the staged root")
	}

	// A checkout without skills leaves the run without a staged root, as before.
	root, _, err = (&Engine{}).stageRuntimeSkills(t.Context(), &agentcore.Agent{}, run, skills.Resolution{}, &agentcore.WorkspaceLease{RootPath: t.TempDir()}, nil)
	if err != nil || root != "" {
		t.Fatalf("expected no staged root without repository skills, got %q %v", root, err)
	}
}
