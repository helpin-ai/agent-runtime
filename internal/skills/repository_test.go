package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRepoSkill(t *testing.T, checkout, dir, name, description, body string) {
	t.Helper()
	skillDir := filepath.Join(checkout, ".agents", "skills", dir)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var doc strings.Builder
	doc.WriteString("---\n")
	if name != "" {
		doc.WriteString("name: " + name + "\n")
	}
	if description != "" {
		doc.WriteString("description: " + description + "\n")
	}
	doc.WriteString("---\n" + body + "\n")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(doc.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readManifest(t *testing.T, destRoot string) []stagedSkillManifestEntry {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(destRoot, stagedSkillManifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Skills []stagedSkillManifestEntry `json:"skills"`
	}
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Skills
}

func TestStageRepositorySkillsDiscoversAndMergesManifest(t *testing.T) {
	checkout, destRoot := t.TempDir(), filepath.Join(t.TempDir(), "skills")
	writeRepoSkill(t, checkout, "release-notes", "release_notes", "Write release notes", "Summarize merged PRs.")
	writeRepoSkill(t, checkout, "db-migrations", "db_migrations", "Author migrations", "Use the migrate command.")
	// A directory below a skill is part of that skill, not a second skill.
	if err := os.MkdirAll(filepath.Join(checkout, ".agents", "skills", "release-notes", "examples"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".agents", "skills", "release-notes", "examples", "SKILL.md"), []byte("---\nname: nested\ndescription: nested\n---\nno"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".agents", "skills", "release-notes", "reference.md"), []byte("reference body"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A pre-existing host manifest must be preserved.
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destRoot, stagedSkillManifestName), []byte(`{"skills":[{"key":"host_skill","source_kind":"workspace","package_dir":"01-host_skill"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := StageRepositorySkills(checkout, destRoot, map[string]bool{"host_skill": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(report.Staged, ",") != "db_migrations,release_notes" {
		t.Fatalf("staged %v", report.Staged)
	}
	entries := readManifest(t, destRoot)
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.Key+":"+entry.SourceKind)
	}
	if strings.Join(keys, ",") != "host_skill:workspace,db_migrations:repository,release_notes:repository" {
		t.Fatalf("manifest entries %v", keys)
	}
	for _, name := range []string{"SKILL.md", "reference.md", filepath.Join("examples", "SKILL.md")} {
		if _, err := os.Stat(filepath.Join(destRoot, "repository", "release_notes", name)); err != nil {
			t.Fatalf("staged copy missing %s: %v", name, err)
		}
	}
	// Re-running replaces repository entries instead of duplicating them.
	if _, err := StageRepositorySkills(checkout, destRoot, map[string]bool{"host_skill": true}); err != nil {
		t.Fatal(err)
	}
	if got := len(readManifest(t, destRoot)); got != 3 {
		t.Fatalf("expected 3 manifest entries after rerun, got %d", got)
	}
}

func TestStageRepositorySkillsSkipsAndLimits(t *testing.T) {
	checkout, destRoot := t.TempDir(), filepath.Join(t.TempDir(), "skills")
	writeRepoSkill(t, checkout, "no-description", "nodesc", "", "body")
	writeRepoSkill(t, checkout, "collides", "host_skill", "Collides with host", "body")
	writeRepoSkill(t, checkout, "good", "good", "Good skill", "body")
	writeRepoSkill(t, checkout, "traversal", "../../escape", "Escapes the stage", "body")
	writeRepoSkill(t, checkout, "hidden", ".hidden", "Hidden name", "body")
	if err := os.MkdirAll(filepath.Join(checkout, ".agents", "skills", "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, ".agents", "skills", "good"), filepath.Join(checkout, ".agents", "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(checkout, ".agents", "skills", "good", "leak")); err != nil {
		t.Fatal(err)
	}
	writeRepoSkill(t, checkout, "huge", "huge", "Too big", "body")
	if err := os.WriteFile(filepath.Join(checkout, ".agents", "skills", "huge", "big.bin"), make([]byte, repositorySkillMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := StageRepositorySkills(checkout, destRoot, map[string]bool{"host_skill": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(report.Staged, ",") != "good" {
		t.Fatalf("only the good skill should stage: %v", report.Staged)
	}
	for _, name := range []string{"no-description", "collides", "huge", "traversal", "hidden"} {
		if _, ok := report.Skipped[name]; !ok {
			t.Fatalf("expected %s to be skipped: %v", name, report.Skipped)
		}
	}
	if _, ok := report.Skipped["linked"]; ok {
		t.Fatal("symlinked skill directories must be ignored silently, not reported")
	}
	if _, err := os.Stat(filepath.Join(destRoot, "repository", "good", "leak")); err == nil {
		t.Fatal("symlink inside a skill must not be copied")
	}
	if _, err := os.Stat(filepath.Join(destRoot, "repository", "huge")); err == nil {
		t.Fatal("oversized skill copy must be removed")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(destRoot)), "escape")); err == nil {
		t.Fatal("a traversal key must never write outside the staged root")
	}

	// Count cap.
	many, manyDest := t.TempDir(), filepath.Join(t.TempDir(), "skills")
	for i := 0; i < repositorySkillsMaxCount+3; i++ {
		writeRepoSkill(t, many, fmt.Sprintf("s%02d", i), fmt.Sprintf("skill_%d", i), fmt.Sprintf("Skill %d", i), "body")
	}
	report, err = StageRepositorySkills(many, manyDest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Limited || len(report.Staged) != repositorySkillsMaxCount {
		t.Fatalf("expected cap of %d with limited flag, got %d limited=%v", repositorySkillsMaxCount, len(report.Staged), report.Limited)
	}
}

func TestStageRepositorySkillsRemovedDirectoryClearsStaleEntries(t *testing.T) {
	checkout, destRoot := t.TempDir(), filepath.Join(t.TempDir(), "skills")
	writeRepoSkill(t, checkout, "old", "old", "Old skill", "body")
	if _, err := StageRepositorySkills(checkout, destRoot, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(checkout, ".agents")); err != nil {
		t.Fatal(err)
	}
	if _, err := StageRepositorySkills(checkout, destRoot, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "repository", "old")); err == nil {
		t.Fatal("stale repository copy must be removed")
	}
	if payload, err := os.ReadFile(filepath.Join(destRoot, stagedSkillManifestName)); err == nil && strings.Contains(string(payload), `"old"`) {
		t.Fatalf("stale manifest entry must be removed: %s", payload)
	}
}

func TestStageRepositorySkillsNoDirectoryIsNoop(t *testing.T) {
	destRoot := filepath.Join(t.TempDir(), "skills")
	report, err := StageRepositorySkills(t.TempDir(), destRoot, nil)
	if err != nil || len(report.Staged) != 0 {
		t.Fatalf("expected no-op, got %v %v", report, err)
	}
	if _, err := os.Stat(destRoot); err == nil {
		t.Fatal("no staged root should be created when there is nothing to stage")
	}
}
