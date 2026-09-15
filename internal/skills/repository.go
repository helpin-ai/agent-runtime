package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SourceRepository marks skills discovered inside the run's repository
// checkout. They are available skills only: never active, no policy, no
// required tools. They carry the same trust as the repository's AGENTS.md.
const SourceRepository = "repository"

const (
	repositorySkillsDir      = ".agents/skills"
	repositoryStageDir       = "repository"
	repositorySkillsMaxCount = 32
	repositorySkillMaxBytes  = 1 << 20
)

// RepositoryStageReport says what repository staging did, for logging.
type RepositoryStageReport struct {
	Staged   []string
	Skipped  map[string]string
	Limited  bool
	Manifest string
}

// StageRepositorySkills discovers `.agents/skills/*/SKILL.md` directly under
// checkoutRoot, copies each accepted skill into destRoot/repository/<key>,
// and merges the entries into the staged manifest so find_skills and
// read_skill see them. reserved holds keys owned by built-in or workspace
// skills; those win on collision. Nothing under a skill is executed.
func StageRepositorySkills(checkoutRoot, destRoot string, reserved map[string]bool) (RepositoryStageReport, error) {
	report := RepositoryStageReport{Skipped: map[string]string{}}
	checkoutRoot = strings.TrimSpace(checkoutRoot)
	destRoot = strings.TrimSpace(destRoot)
	if checkoutRoot == "" || destRoot == "" {
		return report, nil
	}
	stageParent := filepath.Join(destRoot, repositoryStageDir)
	// A retained checkout may hold a previous run's copies and manifest
	// entries; rebuild them from the current checkout, even when the skills
	// directory has since been removed.
	if err := os.RemoveAll(stageParent); err != nil {
		return report, fmt.Errorf("clear repository skill stage: %w", err)
	}
	sourceDir := filepath.Join(checkoutRoot, filepath.FromSlash(repositorySkillsDir))
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return report, mergeRepositoryManifest(destRoot, nil)
		}
		return report, fmt.Errorf("read repository skills: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	checkoutFS := os.DirFS(checkoutRoot)
	var definitions []Definition
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		if len(definitions) >= repositorySkillsMaxCount {
			report.Limited = true
			break
		}
		packagePath := path.Join(repositorySkillsDir, name)
		if info, err := os.Lstat(filepath.Join(sourceDir, name, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
			continue
		}
		definition, err := LoadPackage(checkoutFS, packagePath, SourceRepository)
		if err != nil {
			report.Skipped[name] = err.Error()
			continue
		}
		if strings.TrimSpace(definition.Key) == "" || strings.TrimSpace(definition.Description) == "" {
			report.Skipped[name] = "missing name or description"
			continue
		}
		// The key becomes a directory name under the staged root; never let
		// repository content choose a path outside it.
		if !safeRepositorySkillKey(definition.Key) {
			report.Skipped[name] = "name must use only letters, digits, '_', '-' or '.' and not start with '.'"
			continue
		}
		if reserved[definition.Key] {
			report.Skipped[name] = "key is owned by a built-in or workspace skill"
			continue
		}
		duplicate := false
		for _, existing := range definitions {
			if existing.Key == definition.Key {
				duplicate = true
				break
			}
		}
		if duplicate {
			report.Skipped[name] = "duplicate key within the repository"
			continue
		}
		// Repository skills are discoverable instructions only.
		definition.Policy = Policy{}
		definition.Interface = Interface{}
		definition.RequiredTools = nil
		definition.SupportedRuntimes = nil
		definition.PackagePath = path.Join(repositoryStageDir, definition.Key)
		if err := copyRepositorySkill(filepath.Join(sourceDir, name), filepath.Join(stageParent, definition.Key)); err != nil {
			report.Skipped[name] = err.Error()
			_ = os.RemoveAll(filepath.Join(stageParent, definition.Key))
			continue
		}
		definitions = append(definitions, definition)
		report.Staged = append(report.Staged, definition.Key)
	}
	if len(definitions) == 0 {
		return report, mergeRepositoryManifest(destRoot, nil)
	}
	report.Manifest = filepath.Join(destRoot, stagedSkillManifestName)
	return report, mergeRepositoryManifest(destRoot, definitions)
}

func safeRepositorySkillKey(key string) bool {
	if key == "" || strings.HasPrefix(key, ".") || len(key) > 128 {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// copyRepositorySkill copies regular files below src into dst, refusing
// symlinks and enforcing the per-skill size cap.
func copyRepositorySkill(src, dst string) error {
	var total int64
	return filepath.WalkDir(src, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, current)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		case entry.IsDir():
			return os.MkdirAll(target, 0o755)
		case !entry.Type().IsRegular():
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > repositorySkillMaxBytes {
			return fmt.Errorf("skill exceeds %d bytes", repositorySkillMaxBytes)
		}
		in, err := os.Open(current)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// mergeRepositoryManifest replaces the repository entries in the staged
// manifest, creating the manifest when host staging did not run.
func mergeRepositoryManifest(destRoot string, definitions []Definition) error {
	manifestPath := filepath.Join(destRoot, stagedSkillManifestName)
	var manifest struct {
		Skills []stagedSkillManifestEntry `json:"skills"`
	}
	existed := false
	if payload, err := os.ReadFile(manifestPath); err == nil {
		existed = true
		if err := json.Unmarshal(payload, &manifest); err != nil {
			return fmt.Errorf("parse staged skill manifest: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read staged skill manifest: %w", err)
	}
	kept := manifest.Skills[:0]
	for _, entry := range manifest.Skills {
		if entry.SourceKind != SourceRepository {
			kept = append(kept, entry)
		}
	}
	for _, definition := range definitions {
		kept = append(kept, stagedSkillManifestEntry{
			Key:         definition.Key,
			Title:       definition.Title,
			Description: definition.Description,
			SourceKind:  SourceRepository,
			PackageDir:  definition.PackagePath,
		})
	}
	// Nothing to stage and no manifest to correct: leave the root untouched.
	// An existing manifest is always rewritten so stale entries disappear.
	if !existed && len(kept) == 0 {
		return nil
	}
	if kept == nil {
		kept = []stagedSkillManifestEntry{}
	}
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return fmt.Errorf("create skill staging root: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"skills": kept})
	if err != nil {
		return fmt.Errorf("marshal staged skill manifest: %w", err)
	}
	return os.WriteFile(manifestPath, payload, 0o644)
}
