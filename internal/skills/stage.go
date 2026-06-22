package skills

import (
	"context"
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
		if err := rewriteStagedSkillRuntimeToolNames(stageDir); err != nil {
			return err
		}
	}
	return nil
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

func rewriteStagedSkillRuntimeToolNames(stageDir string) error {
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
		rendered := RenderRuntimeToolNamesInInstructions(string(payload))
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
