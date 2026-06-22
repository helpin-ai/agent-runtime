package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaskRepoSkillRootsHidesAndRestoresRepoSkillDirectories(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".agents", "skills", "demo"), 0o755); err != nil {
		t.Fatalf("mkdir .agents skills: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, ".codex", "skills", "legacy"), 0o755); err != nil {
		t.Fatalf("mkdir .codex skills: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".agents", "config.toml"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write sibling config: %v", err)
	}

	mask, err := MaskRepoSkillRoots(workDir, "run-123")
	if err != nil {
		t.Fatalf("mask repo skill roots: %v", err)
	}
	if mask == nil {
		t.Fatal("expected repo skill mask")
	}
	if _, err := os.Stat(filepath.Join(workDir, ".agents", "skills")); !os.IsNotExist(err) {
		t.Fatalf("expected .agents/skills to be hidden, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".codex", "skills")); !os.IsNotExist(err) {
		t.Fatalf("expected .codex/skills to be hidden, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".agents", "config.toml")); err != nil {
		t.Fatalf("expected sibling .agents config to remain, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".agents", ".helpin-hidden-skills-run-123")); err != nil {
		t.Fatalf("expected hidden .agents skills dir, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".codex", ".helpin-hidden-skills-run-123")); err != nil {
		t.Fatalf("expected hidden .codex skills dir, stat err=%v", err)
	}

	if err := mask.Restore(); err != nil {
		t.Fatalf("restore repo skill roots: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".agents", "skills", "demo")); err != nil {
		t.Fatalf("expected .agents skills to be restored, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".codex", "skills", "legacy")); err != nil {
		t.Fatalf("expected .codex skills to be restored, stat err=%v", err)
	}
}

func TestMaskRepoSkillRootsIgnoresNonDirectoryCodexPath(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".agents", "skills", "demo"), 0o755); err != nil {
		t.Fatalf("mkdir .agents skills: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".codex"), []byte("legacy"), 0o644); err != nil {
		t.Fatalf("write .codex file: %v", err)
	}

	mask, err := MaskRepoSkillRoots(workDir, "run-123")
	if err != nil {
		t.Fatalf("mask repo skill roots: %v", err)
	}
	if mask == nil {
		t.Fatal("expected repo skill mask")
	}
	if _, err := os.Stat(filepath.Join(workDir, ".agents", "skills")); !os.IsNotExist(err) {
		t.Fatalf("expected .agents/skills to be hidden, stat err=%v", err)
	}
	if data, err := os.ReadFile(filepath.Join(workDir, ".codex")); err != nil {
		t.Fatalf("read .codex file: %v", err)
	} else if string(data) != "legacy" {
		t.Fatalf("expected .codex file to remain unchanged, got %q", string(data))
	}

	if err := mask.Restore(); err != nil {
		t.Fatalf("restore repo skill roots: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".agents", "skills", "demo")); err != nil {
		t.Fatalf("expected .agents skills to be restored, stat err=%v", err)
	}
	if data, err := os.ReadFile(filepath.Join(workDir, ".codex")); err != nil {
		t.Fatalf("read restored .codex file: %v", err)
	} else if string(data) != "legacy" {
		t.Fatalf("expected .codex file to remain unchanged after restore, got %q", string(data))
	}
}
