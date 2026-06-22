package runtime

import (
	"path/filepath"
	"strings"
)

type CodexFileChange struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}

type CodexThreadItem struct {
	Type    string            `json:"type"`
	Cwd     string            `json:"cwd,omitempty"`
	Changes []CodexFileChange `json:"changes,omitempty"`
}

func CodexDiffFromFileChange(workDir string, item CodexThreadItem) string {
	if len(item.Changes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(item.Changes))
	for _, change := range item.Changes {
		diff := strings.TrimSpace(change.Diff)
		if diff == "" {
			continue
		}
		normalizedPath := NormalizeCodexDiffPath(workDir, change.Path, item.Cwd)
		if normalizedPath != "" && !strings.HasPrefix(diff, "---") && !strings.HasPrefix(diff, "diff ") {
			diff = "--- a/" + normalizedPath + "\n+++ b/" + normalizedPath + "\n" + diff
		}
		diff = NormalizeCodexUnifiedDiff(workDir, diff, item.Cwd)
		parts = append(parts, diff)
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func NormalizeCodexDiffPath(workDir, rawPath string, extraRoots ...string) string {
	cleaned := strings.TrimSpace(rawPath)
	if cleaned == "" {
		return ""
	}
	for _, root := range append([]string{strings.TrimSpace(workDir)}, extraRoots...) {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		rel, ok := codexRelativePathWithinRoot(root, cleaned)
		if ok {
			return rel
		}
	}
	return codexDiffPathToSlashes(cleaned)
}

func codexRelativePathWithinRoot(root, target string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return "", false
	}
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return codexDiffPathToSlashes(rel), true
}

func codexDiffPathToSlashes(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(value))
}

func NormalizeCodexUnifiedDiff(workDir, diff string, extraRoots ...string) string {
	if strings.TrimSpace(diff) == "" {
		return ""
	}
	lines := strings.Split(diff, "\n")
	for index, line := range lines {
		switch {
		case strings.HasPrefix(line, "--- a/"):
			lines[index] = "--- a/" + NormalizeCodexDiffPath(workDir, strings.TrimPrefix(line, "--- a/"), extraRoots...)
		case strings.HasPrefix(line, "+++ b/"):
			lines[index] = "+++ b/" + NormalizeCodexDiffPath(workDir, strings.TrimPrefix(line, "+++ b/"), extraRoots...)
		case strings.HasPrefix(line, "rename from "):
			lines[index] = "rename from " + NormalizeCodexDiffPath(workDir, strings.TrimPrefix(line, "rename from "), extraRoots...)
		case strings.HasPrefix(line, "rename to "):
			lines[index] = "rename to " + NormalizeCodexDiffPath(workDir, strings.TrimPrefix(line, "rename to "), extraRoots...)
		case strings.HasPrefix(line, "copy from "):
			lines[index] = "copy from " + NormalizeCodexDiffPath(workDir, strings.TrimPrefix(line, "copy from "), extraRoots...)
		case strings.HasPrefix(line, "copy to "):
			lines[index] = "copy to " + NormalizeCodexDiffPath(workDir, strings.TrimPrefix(line, "copy to "), extraRoots...)
		case strings.HasPrefix(line, "diff --git a/"):
			lines[index] = normalizeCodexDiffGitHeader(workDir, line, extraRoots...)
		}
	}
	return strings.Join(lines, "\n")
}

func normalizeCodexDiffGitHeader(workDir, line string, extraRoots ...string) string {
	rest := strings.TrimPrefix(line, "diff --git a/")
	separator := " b/"
	idx := strings.Index(rest, separator)
	if idx <= 0 {
		return line
	}
	left := NormalizeCodexDiffPath(workDir, rest[:idx], extraRoots...)
	right := NormalizeCodexDiffPath(workDir, rest[idx+len(separator):], extraRoots...)
	return "diff --git a/" + left + " b/" + right
}
