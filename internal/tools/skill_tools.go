package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxSkillReadBytes = 96 * 1024

const stagedSkillManifestName = ".available-skills.json"

type skillListFilesInput struct {
	Path string `json:"path,omitempty"`
}

type skillReadFileInput struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type availableSkillSearchInput struct {
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type availableSkillReadInput struct {
	Key      string `json:"key,omitempty"`
	SkillID  string `json:"skill_id,omitempty"`
	Path     string `json:"path,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type stagedAvailableSkill struct {
	Key               string   `json:"key"`
	SkillID           string   `json:"skill_id,omitempty"`
	VersionKey        string   `json:"version_key,omitempty"`
	Title             string   `json:"title,omitempty"`
	Description       string   `json:"description,omitempty"`
	SourceKind        string   `json:"source_kind,omitempty"`
	RequiredTools     []string `json:"required_tools,omitempty"`
	SupportedRuntimes []string `json:"supported_runtimes,omitempty"`
	PackageDir        string   `json:"package_dir,omitempty"`
}

func RegisterSkillTools(r *Registry) {
	if r == nil {
		return
	}
	registerAvailableSkillTools(r)
	r.Register(Definition{
		Name:        "skills.list_files",
		Description: "List files in the current run's staged agent-runtime skill packages. Use this before reading skill references or examples.",
		Category:    "Skills",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "Optional relative directory inside the staged skill root."},
			},
			"additionalProperties": false,
		},
		Mutating: false,
	}, func(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
		var req skillListFilesInput
		if len(input) > 0 {
			if err := json.Unmarshal(input, &req); err != nil {
				return nil, fmt.Errorf("decode skills.list_files input: %w", err)
			}
		}
		root, err := stagedSkillRoot(callCtx)
		if err != nil {
			return nil, err
		}
		dir, rel, err := resolveStagedSkillPath(root, req.Path)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, fmt.Errorf("list staged skills: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("staged skill path %q is not a directory", rel)
		}
		entries, err := listStagedSkillFiles(root, dir)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{
			"root":  ".agent-runtime/skills",
			"path":  rel,
			"files": entries,
		})
	})
	r.Register(Definition{
		Name:        "skills.read_file",
		Description: "Read a file from the current run's staged agent-runtime skill packages. Paths must come from skills.list_files.",
		Category:    "Skills",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":      map[string]any{"type": "string", "description": "Relative file path inside the staged skill root."},
				"max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": maxSkillReadBytes, "description": "Optional maximum bytes to return."},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
		Mutating: false,
	}, func(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
		var req skillReadFileInput
		if err := json.Unmarshal(input, &req); err != nil {
			return nil, fmt.Errorf("decode skills.read_file input: %w", err)
		}
		root, err := stagedSkillRoot(callCtx)
		if err != nil {
			return nil, err
		}
		filePath, rel, err := resolveStagedSkillPath(root, req.Path)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(filePath)
		if err != nil {
			return nil, fmt.Errorf("read staged skill file: %w", err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("staged skill path %q is a directory", rel)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("staged skill path %q is a symlink", rel)
		}
		limit := req.MaxBytes
		if limit <= 0 || limit > maxSkillReadBytes {
			limit = maxSkillReadBytes
		}
		payload, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read staged skill file: %w", err)
		}
		truncated := false
		if len(payload) > limit {
			payload = payload[:limit]
			truncated = true
		}
		return json.Marshal(map[string]any{
			"path":      rel,
			"content":   string(payload),
			"bytes":     len(payload),
			"truncated": truncated,
			"max_bytes": limit,
			"root":      ".agent-runtime/skills",
		})
	})
}

func registerAvailableSkillTools(r *Registry) {
	r.Register(Definition{
		Name:        "list_available_skills",
		Description: "List optional skills available to this agent. Returns metadata only; use read_skill for one selected skill.",
		Category:    "Skills",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"required":             []string{},
			"additionalProperties": false,
		},
		Mutating: false,
	}, func(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
		if err := decodeEmptyObject(input, "list_available_skills"); err != nil {
			return nil, err
		}
		skills, err := loadAvailableSkillManifest(callCtx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"total": len(skills), "skills": publicAvailableSkills(skills)})
	})

	r.Register(Definition{
		Name:        "search_available_skills",
		Description: "Search optional skills available to this agent by key, title, description, source, runtime, or required tool.",
		Category:    "Skills",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Optional case-insensitive search text."},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "Maximum results; defaults to 10."},
			},
			"required":             []string{},
			"additionalProperties": false,
		},
		Mutating: false,
	}, func(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
		var req availableSkillSearchInput
		if len(input) > 0 {
			if err := json.Unmarshal(input, &req); err != nil {
				return nil, fmt.Errorf("parse search_available_skills input: %w", err)
			}
		}
		limit := req.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			return nil, fmt.Errorf("limit must be at most 50")
		}
		skills, err := loadAvailableSkillManifest(callCtx)
		if err != nil {
			return nil, err
		}
		query := strings.ToLower(strings.TrimSpace(req.Query))
		matches := make([]stagedAvailableSkill, 0, len(skills))
		for _, skill := range skills {
			if query == "" || availableSkillMatches(skill, query) {
				matches = append(matches, skill)
			}
		}
		total := len(matches)
		if len(matches) > limit {
			matches = matches[:limit]
		}
		return json.Marshal(map[string]any{"query": strings.TrimSpace(req.Query), "total": total, "skills": publicAvailableSkills(matches)})
	})

	r.Register(Definition{
		Name:        "read_skill",
		Description: "Read one optional skill by key or skill_id. Defaults to SKILL.md and can read a relative referenced file from that package.",
		Category:    "Skills",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":       map[string]any{"type": "string", "description": "Available skill key."},
				"skill_id":  map[string]any{"type": "string", "description": "Workspace skill ID when a key is ambiguous."},
				"path":      map[string]any{"type": "string", "description": "Optional path inside the selected package; defaults to SKILL.md."},
				"max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": maxSkillReadBytes, "description": "Optional maximum bytes to return."},
			},
			"required":             []string{},
			"additionalProperties": false,
		},
		Mutating: false,
	}, func(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
		var req availableSkillReadInput
		if err := json.Unmarshal(input, &req); err != nil {
			return nil, fmt.Errorf("parse read_skill input: %w", err)
		}
		req.Key = strings.TrimSpace(req.Key)
		req.SkillID = strings.TrimSpace(req.SkillID)
		if (req.Key == "") == (req.SkillID == "") {
			return nil, fmt.Errorf("exactly one of key or skill_id is required")
		}
		skills, err := loadAvailableSkillManifest(callCtx)
		if err != nil {
			return nil, err
		}
		matches := make([]stagedAvailableSkill, 0, 1)
		for _, skill := range skills {
			if (req.Key != "" && skill.Key == req.Key) || (req.SkillID != "" && skill.SkillID == req.SkillID) {
				matches = append(matches, skill)
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("skill is not available to this agent")
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("multiple available skills use key %q; use skill_id", req.Key)
		}
		return readAvailableSkillFile(callCtx, matches[0], req)
	})
}

func stagedSkillRoot(callCtx CallContext) (string, error) {
	if root := strings.TrimSpace(callCtx.StagedSkillRoot); root != "" {
		if info, err := os.Stat(root); err != nil {
			return "", fmt.Errorf("staged skill root is unavailable: %w", err)
		} else if !info.IsDir() {
			return "", fmt.Errorf("staged skill root is not a directory")
		}
		return root, nil
	}
	if callCtx.Run == nil || callCtx.Run.WorkspaceLease == nil {
		return "", fmt.Errorf("staged skill root is unavailable because the run has no workspace lease")
	}
	root := filepath.Join(strings.TrimSpace(callCtx.Run.WorkspaceLease.RootPath), ".agent-runtime", "skills")
	if strings.TrimSpace(callCtx.Run.WorkspaceLease.RootPath) == "" {
		return "", fmt.Errorf("staged skill root is unavailable because the workspace lease has no root path")
	}
	if info, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("staged skill root is unavailable: %w", err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("staged skill root is not a directory")
	}
	return root, nil
}

func resolveStagedSkillPath(root, requested string) (string, string, error) {
	requested = strings.TrimSpace(filepath.ToSlash(requested))
	if requested == "" {
		requested = "."
	}
	if strings.HasPrefix(requested, "/") {
		return "", "", fmt.Errorf("staged skill paths must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(requested))
	if clean == "." {
		return root, ".", nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("staged skill path escapes the skill root")
	}
	full := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", "", fmt.Errorf("resolve staged skill path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("staged skill path escapes the skill root")
	}
	return full, filepath.ToSlash(rel), nil
}

func listStagedSkillFiles(root, dir string) ([]map[string]any, error) {
	var entries []map[string]any
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		item := map[string]any{
			"path": filepath.ToSlash(rel),
			"type": "file",
			"size": info.Size(),
		}
		if entry.IsDir() {
			item["type"] = "directory"
			item["size"] = int64(0)
		}
		entries = append(entries, item)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk staged skill files: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return fmt.Sprint(entries[i]["path"]) < fmt.Sprint(entries[j]["path"])
	})
	return entries, nil
}

func decodeEmptyObject(input json.RawMessage, toolName string) error {
	if len(input) == 0 {
		return nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(input, &values); err != nil {
		return fmt.Errorf("parse %s input: %w", toolName, err)
	}
	if len(values) > 0 {
		return fmt.Errorf("%s does not accept fields", toolName)
	}
	return nil
}

func loadAvailableSkillManifest(callCtx CallContext) ([]stagedAvailableSkill, error) {
	root, err := stagedSkillRoot(callCtx)
	if err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(filepath.Join(root, stagedSkillManifestName))
	if err != nil {
		return nil, fmt.Errorf("available skill manifest is unavailable: %w", err)
	}
	var manifest struct {
		Skills []stagedAvailableSkill `json:"skills"`
	}
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return nil, fmt.Errorf("parse available skill manifest: %w", err)
	}
	sort.Slice(manifest.Skills, func(i, j int) bool {
		if manifest.Skills[i].Key == manifest.Skills[j].Key {
			return manifest.Skills[i].SkillID < manifest.Skills[j].SkillID
		}
		return manifest.Skills[i].Key < manifest.Skills[j].Key
	})
	return manifest.Skills, nil
}

func publicAvailableSkills(skills []stagedAvailableSkill) []map[string]any {
	out := make([]map[string]any, 0, len(skills))
	for _, skill := range skills {
		entry := map[string]any{
			"key":                skill.Key,
			"title":              skill.Title,
			"description":        skill.Description,
			"source_kind":        skill.SourceKind,
			"required_tools":     skill.RequiredTools,
			"supported_runtimes": skill.SupportedRuntimes,
		}
		if skill.SkillID != "" {
			entry["skill_id"] = skill.SkillID
		}
		if skill.VersionKey != "" {
			entry["version_key"] = skill.VersionKey
		}
		out = append(out, entry)
	}
	return out
}

func availableSkillMatches(skill stagedAvailableSkill, query string) bool {
	haystack := strings.ToLower(strings.Join([]string{
		skill.Key,
		skill.Title,
		skill.Description,
		skill.SourceKind,
		strings.Join(skill.RequiredTools, " "),
		strings.Join(skill.SupportedRuntimes, " "),
	}, " "))
	return strings.Contains(haystack, query)
}

func readAvailableSkillFile(callCtx CallContext, skill stagedAvailableSkill, req availableSkillReadInput) (json.RawMessage, error) {
	root, err := stagedSkillRoot(callCtx)
	if err != nil {
		return nil, err
	}
	packageRoot, _, err := resolveStagedSkillPath(root, skill.PackageDir)
	if err != nil {
		return nil, err
	}
	requested := strings.TrimSpace(req.Path)
	if requested == "" {
		requested = "SKILL.md"
	}
	filePath, rel, err := resolveStagedSkillPath(packageRoot, requested)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(filePath)
	if err != nil {
		return nil, fmt.Errorf("read skill file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("skill path %q is a directory", rel)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("skill path %q is a symlink", rel)
	}
	limit := req.MaxBytes
	if limit <= 0 {
		limit = maxSkillReadBytes
	}
	if limit > maxSkillReadBytes {
		return nil, fmt.Errorf("max_bytes must be at most %d", maxSkillReadBytes)
	}
	payload, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read skill file: %w", err)
	}
	truncated := len(payload) > limit
	if truncated {
		payload = payload[:limit]
	}
	return json.Marshal(map[string]any{
		"key":         skill.Key,
		"skill_id":    skill.SkillID,
		"version_key": skill.VersionKey,
		"path":        filepath.ToSlash(rel),
		"content":     string(payload),
		"bytes":       len(payload),
		"truncated":   truncated,
		"max_bytes":   limit,
	})
}
