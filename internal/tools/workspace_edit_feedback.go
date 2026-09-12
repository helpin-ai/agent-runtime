package tools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	workspaceEditDiffLines  = 60
	workspaceEditDiffRunes  = 3000
	workspaceEditDiffMarker = "[diff truncated]"
)

// An exact replacement changes one contiguous region. Trim common whole lines
// to show that region with context, without running a quadratic diff algorithm.
// Keep line endings during comparison so newline-only changes remain visible.
func workspaceEditDiff(before, after string) string {
	oldLines, newLines := editFeedbackLines(before), editFeedbackLines(after)
	start := 0
	for start < len(oldLines) && start < len(newLines) && oldLines[start] == newLines[start] {
		start++
	}
	oldEnd, newEnd := len(oldLines), len(newLines)
	for oldEnd > start && newEnd > start && oldLines[oldEnd-1] == newLines[newEnd-1] {
		oldEnd--
		newEnd--
	}
	contextStart := max(0, start-3)
	contextEnd := min(len(oldLines), oldEnd+3)
	oldCount := contextEnd - contextStart
	newCount := newEnd + (contextEnd - oldEnd) - contextStart
	oldLine, newLine := contextStart+1, contextStart+1
	if oldCount == 0 {
		oldLine--
	}
	if newCount == 0 {
		newLine--
	}

	var excerpt []string
	runes := 0
	truncated := false
	// Reserve one line and enough characters for the truncation marker.
	emit := func(line string) bool {
		remaining := workspaceEditDiffRunes - len(workspaceEditDiffMarker) - 1 - runes
		if len(excerpt) > 0 {
			remaining--
		}
		if len(excerpt) >= workspaceEditDiffLines-1 || remaining <= 0 {
			truncated = true
			return false
		}
		if utf8.RuneCountInString(line) > remaining {
			line = truncateReadRunes(line, remaining)
			truncated = true
		}
		if len(excerpt) > 0 {
			runes++
		}
		excerpt = append(excerpt, line)
		runes += utf8.RuneCountInString(line)
		return !truncated
	}
	emit(fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldLine, oldCount, newLine, newCount))
	groups := []struct {
		prefix string
		lines  []string
	}{
		{" ", oldLines[contextStart:start]},
		{"-", oldLines[start:oldEnd]},
		{"+", newLines[start:newEnd]},
		{" ", oldLines[oldEnd:contextEnd]},
	}
render:
	for _, group := range groups {
		for _, line := range group.lines {
			if !emit(group.prefix + strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")) {
				break render
			}
			if !strings.HasSuffix(line, "\n") && !emit("\\ No newline at end of file") {
				break render
			}
		}
	}
	if truncated {
		excerpt = append(excerpt, workspaceEditDiffMarker)
	}
	return strings.Join(excerpt, "\n")
}

func editFeedbackLines(content string) []string {
	lines := strings.SplitAfter(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
