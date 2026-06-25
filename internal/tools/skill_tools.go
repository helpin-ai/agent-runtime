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

type skillListFilesInput struct {
	Path string `json:"path,omitempty"`
}

type skillReadFileInput struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

func RegisterSkillTools(r *Registry) {
	if r == nil {
		return
	}
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
