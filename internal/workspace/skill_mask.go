package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

type RepoSkillMask struct {
	moves []repoSkillMove
}

type repoSkillMove struct {
	originalPath string
	hiddenPath   string
}

var repoSkillMaskSuffixPattern = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func MaskRepoSkillRoots(workDir, runID string) (*RepoSkillMask, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil, nil
	}
	suffix := sanitizeRepoSkillMaskSuffix(runID)
	if suffix == "" {
		suffix = "run"
	}
	mask := &RepoSkillMask{}
	for _, relativePath := range []string{
		filepath.Join(".agents", "skills"),
		filepath.Join(".codex", "skills"),
	} {
		originalPath := filepath.Join(workDir, relativePath)
		info, err := os.Lstat(originalPath)
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) || isPathComponentNotDirectory(err) {
				continue
			}
			_ = mask.Restore()
			return nil, fmt.Errorf("stat repo skill root %q: %w", relativePath, err)
		}
		if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		hiddenPath := filepath.Join(filepath.Dir(originalPath), ".helpin-hidden-skills-"+suffix)
		if err := os.RemoveAll(hiddenPath); err != nil {
			_ = mask.Restore()
			return nil, fmt.Errorf("clear hidden repo skill root %q: %w", hiddenPath, err)
		}
		if err := os.Rename(originalPath, hiddenPath); err != nil {
			_ = mask.Restore()
			return nil, fmt.Errorf("hide repo skill root %q: %w", relativePath, err)
		}
		mask.moves = append(mask.moves, repoSkillMove{originalPath: originalPath, hiddenPath: hiddenPath})
	}
	if len(mask.moves) == 0 {
		return nil, nil
	}
	return mask, nil
}

func (m *RepoSkillMask) Restore() error {
	if m == nil {
		return nil
	}
	var firstErr error
	for idx := len(m.moves) - 1; idx >= 0; idx-- {
		move := m.moves[idx]
		if _, err := os.Lstat(move.hiddenPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := os.RemoveAll(move.originalPath); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := os.Rename(move.hiddenPath, move.originalPath); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func isPathComponentNotDirectory(err error) bool {
	if err == nil {
		return false
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return false
	}
	return errors.Is(pathErr.Err, os.ErrNotExist) || errors.Is(pathErr.Err, syscall.ENOTDIR)
}

func sanitizeRepoSkillMaskSuffix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = repoSkillMaskSuffixPattern.ReplaceAllString(value, "-")
	return strings.Trim(value, "-")
}
