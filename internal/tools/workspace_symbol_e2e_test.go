package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestReadSymbolAgainstThisRepository is the end-to-end check from the plan: it
// points the tools at agent-runtime's own source and resolves a declaration
// whose true span is known independently. It guards against the extractor
// drifting on real code rather than tidy fixtures.
func TestReadSymbolAgainstThisRepository(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	target := filepath.Join(repoRoot, "internal", "tools", "workspace_read_tools.go")
	if _, err := os.Stat(target); err != nil {
		t.Skipf("repository source not available: %v", err)
	}

	registry, callCtx := workspaceToolTestRegistry(t)
	callCtx.Run.WorkspaceLease.RootPath = repoRoot
	relPath := filepath.ToSlash(filepath.Join("internal", "tools", "workspace_read_tools.go"))

	input, err := json.Marshal(map[string]string{"path": relPath, "symbol": "readTextFileWindow"})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	raw, err := registry.Execute(context.Background(), callCtx, "read_symbol", input)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	out := workspaceToolString(t, raw)

	if !strings.Contains(out, "method readTextFileWindow in") {
		t.Errorf("unexpected header:\n%s", firstLines(out, 3))
	}
	if !strings.Contains(out, "func (p *workspaceToolPack) readTextFileWindow(") {
		t.Errorf("declaration line missing:\n%s", firstLines(out, 6))
	}
	// The true span is reported in the header even when the body is cut short,
	// so the caller learns the declaration's real extent in one call. Validate
	// the reported span against the file itself rather than hard-coding line
	// numbers that would rot on the next edit.
	start, end := parseReportedSpan(t, out)
	source, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	sourceLines := strings.Split(string(source), "\n")
	if start < 1 || end > len(sourceLines) || start >= end {
		t.Fatalf("implausible span %d-%d for a %d-line file", start, end, len(sourceLines))
	}
	if got := sourceLines[start-1]; !strings.HasPrefix(got, "func (p *workspaceToolPack) readTextFileWindow(") {
		t.Errorf("span starts at line %d = %q, want the declaration line", start, got)
	}
	// A Go top-level declaration ends on its own closing brace at column 0.
	if got := sourceLines[end-1]; got != "}" {
		t.Errorf("span ends at line %d = %q, want a closing brace", end, got)
	}
	// Being budget-bound, it must hand back a continuation offset inside the
	// span rather than silently stopping.
	if !strings.Contains(out, "offset_line=") {
		t.Errorf("truncated read must advertise a continuation:\n%s", out)
	}
	// The following declaration must never bleed in.
	if strings.Contains(out, "func readStreamHasMore(") {
		t.Errorf("span overran into the following declaration:\n%s", firstLines(out, 6))
	}
}

// TestListSymbolsAgainstThisRepository checks the outline on real source and
// asserts the specific improvement over the previous scan: an end line.
func TestListSymbolsAgainstThisRepository(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "internal", "tools", "workspace_read_tools.go")); err != nil {
		t.Skipf("repository source not available: %v", err)
	}

	registry, callCtx := workspaceToolTestRegistry(t)
	callCtx.Run.WorkspaceLease.RootPath = repoRoot

	input, err := json.Marshal(map[string]string{
		"path": filepath.ToSlash(filepath.Join("internal", "tools", "workspace_read_tools.go")),
	})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	raw, err := registry.Execute(context.Background(), callCtx, "list_symbols", input)
	if err != nil {
		t.Fatalf("list_symbols: %v", err)
	}
	out := workspaceToolString(t, raw)

	for _, want := range []string{
		"[method readTextFileWindow, lines ",
		"[function normalizeReadFileWindow, lines ",
		"[function formatReadFileWindow, lines ",
		"[type readFileWindow, lines ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("outline missing %q", want)
		}
	}
}

// parseReportedSpan pulls the "(lines N-M)" span out of read_symbol's header.
func parseReportedSpan(t *testing.T, out string) (int, int) {
	t.Helper()
	header := firstLines(out, 1)
	open := strings.LastIndex(header, "(lines ")
	if open < 0 || !strings.HasSuffix(header, ")") {
		t.Fatalf("header carries no span: %q", header)
	}
	span := header[open+len("(lines ") : len(header)-1]
	parts := strings.SplitN(span, "-", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed span %q", span)
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		t.Fatalf("malformed span start %q: %v", parts[0], err)
	}
	end, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("malformed span end %q: %v", parts[1], err)
	}
	return start, end
}

func firstLines(text string, count int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}
