package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type PackageStore interface {
	GetObject(ctx context.Context, key string) ([]byte, error)
}

type StageOptions struct {
	Lookup        WorkspaceLookup
	PackageStore  PackageStore
	LookupContext LookupContext
	DestRoot      string
	RuntimeKind   string
}

var stagedSkillNamePattern = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func StageResolvedInto(ctx context.Context, resolution Resolution, opts StageOptions) error {
	if len(resolution.CoreRefs) == 0 || len(resolution.Definitions) == 0 {
		return nil
	}
	if len(resolution.CoreRefs) != len(resolution.Definitions) {
		return fmt.Errorf("skill refs and definitions length mismatch")
	}
	destRoot := strings.TrimSpace(opts.DestRoot)
	if destRoot == "" {
		return fmt.Errorf("skill staging root is required")
	}
	if err := os.RemoveAll(destRoot); err != nil {
		return fmt.Errorf("clear skill staging root: %w", err)
	}
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return fmt.Errorf("create skill staging root: %w", err)
	}

	for index, ref := range resolution.CoreRefs {
		definition := resolution.Definitions[index]
		stageDir := filepath.Join(destRoot, stagedSkillDirName(index, definition.Key))
		switch {
		case strings.TrimSpace(ref.SkillID) != "":
			if err := stageWorkspaceSkill(ctx, ref, definition, opts, stageDir); err != nil {
				return err
			}
		default:
			if err := CopyBuiltInSkillPackageToDir(definition.Key, stageDir); err != nil {
				return fmt.Errorf("stage built-in skill %q: %w", definition.Key, err)
			}
		}
		if err := rewriteStagedSkillRuntimeToolNames(stageDir, opts.RuntimeKind); err != nil {
			return err
		}
	}
	return writeStagedSkillManifest(destRoot, resolution)
}

const stagedSkillManifestName = ".available-skills.json"

type stagedSkillManifestEntry struct {
	Key               string   `json:"key"`
	SkillID           string   `json:"skill_id,omitempty"`
	VersionKey        string   `json:"version_key,omitempty"`
	Title             string   `json:"title,omitempty"`
	Description       string   `json:"description,omitempty"`
	SourceKind        string   `json:"source_kind,omitempty"`
	RequiredTools     []string `json:"required_tools,omitempty"`
	SupportedRuntimes []string `json:"supported_runtimes,omitempty"`
	PackageDir        string   `json:"package_dir"`
}

func writeStagedSkillManifest(destRoot string, resolution Resolution) error {
	entries := make([]stagedSkillManifestEntry, 0, len(resolution.CoreRefs))
	for index, ref := range resolution.CoreRefs {
		definition := resolution.Definitions[index]
		entries = append(entries, stagedSkillManifestEntry{
			Key:               strings.TrimSpace(definition.Key),
			SkillID:           strings.TrimSpace(ref.SkillID),
			VersionKey:        strings.TrimSpace(ref.VersionKey),
			Title:             strings.TrimSpace(definition.Title),
			Description:       strings.TrimSpace(definition.Description),
			SourceKind:        strings.TrimSpace(definition.SourceKind),
			RequiredTools:     append([]string(nil), definition.RequiredTools...),
			SupportedRuntimes: append([]string(nil), definition.SupportedRuntimes...),
			PackageDir:        stagedSkillDirName(index, definition.Key),
		})
	}
	payload, err := json.Marshal(map[string]any{"skills": entries})
	if err != nil {
		return fmt.Errorf("marshal staged skill manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destRoot, stagedSkillManifestName), payload, 0o644); err != nil {
		return fmt.Errorf("write staged skill manifest: %w", err)
	}
	return nil
}

// ReconcileResolutionFromStagedPackages makes the versioned package content
// authoritative for the run-time skill contract. Workspace lookup metadata is
// useful for discovery, but it can lag behind the package and must not be able
// to silently drop required tools or completion-interaction policy that Codex
// reads from the staged SKILL.md package.
func ReconcileResolutionFromStagedPackages(resolution Resolution, destRoot string) (Resolution, error) {
	if len(resolution.CoreRefs) == 0 || len(resolution.Definitions) == 0 {
		return resolution, nil
	}
	if len(resolution.CoreRefs) != len(resolution.Definitions) {
		return Resolution{}, fmt.Errorf("skill refs and definitions length mismatch")
	}
	destRoot = strings.TrimSpace(destRoot)
	if destRoot == "" {
		return Resolution{}, fmt.Errorf("staged skill root is required")
	}

	reconciled := resolution
	reconciled.Definitions = append([]Definition(nil), resolution.Definitions...)
	stagedFS := os.DirFS(destRoot)
	for index, lookupDefinition := range resolution.Definitions {
		// Embedded built-ins were already loaded from the same package source by
		// the registry. Reconciliation is needed for host-provided, versioned
		// packages whose lookup projection can lag behind their archive.
		if strings.TrimSpace(resolution.CoreRefs[index].SkillID) == "" {
			continue
		}
		packagePath := stagedSkillDirName(index, lookupDefinition.Key)
		packageDefinition, err := LoadPackage(stagedFS, packagePath, lookupDefinition.SourceKind)
		if err != nil {
			return Resolution{}, fmt.Errorf("load staged skill package %q: %w", lookupDefinition.Key, err)
		}
		if lookupDefinition.Key != "" && packageDefinition.Key != lookupDefinition.Key {
			return Resolution{}, fmt.Errorf(
				"staged skill package key mismatch: lookup declared %q but package declared %q",
				lookupDefinition.Key,
				packageDefinition.Key,
			)
		}
		reconciled.Definitions[index] = mergeLookupAndPackageDefinition(lookupDefinition, packageDefinition)
	}
	reconciled.Instructions = CompileInstructions(reconciled.Definitions)
	reconciled.Policy = AggregatePolicy(reconciled.Definitions)
	return reconciled, nil
}

func mergeLookupAndPackageDefinition(lookupDefinition, packageDefinition Definition) Definition {
	merged := packageDefinition
	merged.SourceKind = firstNonEmpty(packageDefinition.SourceKind, lookupDefinition.SourceKind)
	merged.RequiredTools = SortedUniqueStrings(append(
		append([]string(nil), lookupDefinition.RequiredTools...),
		packageDefinition.RequiredTools...,
	))
	if len(merged.SupportedRuntimes) == 0 {
		merged.SupportedRuntimes = append([]string(nil), lookupDefinition.SupportedRuntimes...)
	}
	merged.Policy = AggregatePolicy([]Definition{lookupDefinition, packageDefinition})
	if merged.Interface == (Interface{}) {
		merged.Interface = lookupDefinition.Interface
	}
	return normalizeDefinition(merged)
}

func stageWorkspaceSkill(ctx context.Context, ref agentcore.SkillRef, definition Definition, opts StageOptions, stageDir string) error {
	if opts.Lookup == nil {
		return fmt.Errorf("workspace skill lookup is not configured")
	}
	if opts.PackageStore == nil {
		return fmt.Errorf("skill package store is not configured")
	}
	skill, err := getWorkspaceSkillByID(ctx, opts.Lookup, opts.LookupContext, ref.SkillID)
	if err != nil {
		return err
	}
	if skill == nil || skill.Archived {
		return fmt.Errorf("workspace skill not found")
	}
	if !versionMatches(RefFromCore(ref), skill.VersionKey) {
		return fmt.Errorf("workspace skill %q version mismatch", skill.Key)
	}
	objectKey := strings.TrimSpace(skill.PackageObjectKey)
	if objectKey == "" {
		return fmt.Errorf("workspace skill %q does not declare package_object_key", skill.Key)
	}
	archiveData, err := opts.PackageStore.GetObject(ctx, objectKey)
	if err != nil {
		return fmt.Errorf("load workspace skill package %q: %w", skill.Key, err)
	}
	if err := ExtractSkillArchiveToDir(archiveData, stageDir); err != nil {
		return fmt.Errorf("stage workspace skill %q: %w", definition.Key, err)
	}
	return nil
}

func rewriteStagedSkillRuntimeToolNames(stageDir, runtimeKind string) error {
	return filepath.WalkDir(stageDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.ToLower(filepath.Ext(path)) != ".md" {
			return nil
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read staged skill markdown %q: %w", path, err)
		}
		rendered := RenderRuntimeToolNamesInInstructionsForRuntime(string(payload), runtimeKind)
		if rendered == string(payload) {
			return nil
		}
		if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
			return fmt.Errorf("write staged skill markdown %q: %w", path, err)
		}
		return nil
	})
}

func stagedSkillDirName(index int, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "skill"
	}
	key = stagedSkillNamePattern.ReplaceAllString(key, "_")
	key = strings.Trim(key, "_")
	if key == "" {
		key = "skill"
	}
	return fmt.Sprintf("%02d-%s", index+1, key)
}
