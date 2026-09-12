package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/symbols"
)

const (
	// maxListedSymbols bounds outline output. Generated files can carry
	// thousands of declarations, and an unbounded list would be truncated
	// downstream with no indication that anything was dropped.
	maxListedSymbols = 300
	// maxSymbolSignatureRunes bounds each rendered declaration line.
	maxSymbolSignatureRunes = 200
	// maxSymbolMatchesReported bounds how many same-named declarations
	// read_symbol mentions when the name is ambiguous.
	maxSymbolMatchesReported = 10
	// maxSymbolDocstringLines bounds how far read_symbol walks backwards over a
	// leading comment block.
	maxSymbolDocstringLines = 40
)

// symbolPattern drives the pre-tree-sitter scan retained as a fallback.
type symbolPattern struct {
	exts     []string
	patterns []*regexp.Regexp
}

// workspaceSymbolPatterns is the legacy line-oriented scan. It is still used for
// file types no grammar is linked in for, so those files keep working rather
// than failing with "unsupported file type".
var workspaceSymbolPatterns = []symbolPattern{
	{exts: []string{".go"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^func\s`), regexp.MustCompile(`^type\s+\w+\s+(struct|interface)`), regexp.MustCompile(`^type\s+\w+\s`), regexp.MustCompile(`^var\s+\w+`), regexp.MustCompile(`^const\s+\w+`)}},
	{exts: []string{".ts", ".tsx"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^export\s+(function|const|class|type|interface|enum)\s`), regexp.MustCompile(`^\s*(public|private|protected|async)\s+\w+\(`), regexp.MustCompile(`^function\s+\w+`)}},
	{exts: []string{".js", ".jsx"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^export\s+(function|const|class)\s`), regexp.MustCompile(`^function\s+\w+`), regexp.MustCompile(`^class\s+\w+`), regexp.MustCompile(`module\.exports`)}},
	{exts: []string{".py"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^def\s+\w+`), regexp.MustCompile(`^class\s+\w+`), regexp.MustCompile(`^async\s+def\s+\w+`)}},
	{exts: []string{".rs"}, patterns: []*regexp.Regexp{regexp.MustCompile(`^pub\s+(fn|struct|enum|trait|type|impl|mod)\s`), regexp.MustCompile(`^fn\s+`), regexp.MustCompile(`^struct\s+`), regexp.MustCompile(`^enum\s+`), regexp.MustCompile(`^trait\s+`), regexp.MustCompile(`^impl\s`)}},
	{exts: []string{".java"}, patterns: []*regexp.Regexp{regexp.MustCompile(`(public|private|protected).*\s+(class|interface|enum)\s+`), regexp.MustCompile(`(public|private|protected)\s+.*\w+\s*\([^)]*\)\s*\{`)}},
}

// commentPrefixes are the line-comment markers used to walk backwards from a
// declaration to its documentation block.
var commentPrefixes = map[string][]string{
	".go": {"//"}, ".ts": {"//", "*", "/*"}, ".tsx": {"//", "*", "/*"},
	".js": {"//", "*", "/*"}, ".jsx": {"//", "*", "/*"},
	".mjs": {"//", "*", "/*"}, ".cjs": {"//", "*", "/*"},
	".rs": {"//", "///", "//!"}, ".java": {"//", "*", "/*"},
	".py": {"#"}, ".pyi": {"#"},
}

// readWorkspaceSourceForSymbols loads a file for parsing. It reuses the read
// path's containment and binary checks so symbol tools cannot reach anything
// read_file could not.
func (p *workspaceToolPack) readWorkspaceSourceForSymbols(root, path string) ([]byte, error) {
	f, _, err := openWorkspaceReadFile(root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file: %s", displayReadPath(path, maxReadDisplayedPathRunes))
	}
	if info.Size() > symbols.MaxParseBytes {
		return nil, fmt.Errorf(
			"file is too large for symbol extraction: %s (%d bytes, max %d)",
			displayReadPath(path, maxReadDisplayedPathRunes), info.Size(), symbols.MaxParseBytes,
		)
	}
	// Cap the read itself rather than trusting the stat: the file could grow
	// between the two, and MaxParseBytes is the parser's real bound.
	content, err := io.ReadAll(io.LimitReader(f, symbols.MaxParseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(content) > symbols.MaxParseBytes {
		return nil, fmt.Errorf(
			"file is too large for symbol extraction: %s (max %d bytes)",
			displayReadPath(path, maxReadDisplayedPathRunes), symbols.MaxParseBytes,
		)
	}
	if isBinaryContent(content) {
		return nil, fmt.Errorf("file appears to be binary, cannot read: %s", displayReadPath(path, maxReadDisplayedPathRunes))
	}
	return content, nil
}

func (p *workspaceToolPack) listSymbols(_ context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Path string `json:"path"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.Path) == "" {
		return nil, fmt.Errorf("path is required")
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "list_symbols", params.repoSelector())
	if err != nil {
		return nil, err
	}
	content, err := p.readWorkspaceSourceForSymbols(root, params.Path)
	if err != nil {
		return nil, err
	}

	if !symbols.Supported(params.Path) {
		return legacyListSymbols(params.Path, content)
	}
	found, err := symbols.Extract(params.Path, content)
	if err != nil {
		// A grammar problem must not take away a capability the legacy scan
		// still provides for this extension.
		if errors.Is(err, symbols.ErrUnsupported) || errors.Is(err, symbols.ErrTooLarge) {
			return legacyListSymbols(params.Path, content)
		}
		return nil, err
	}
	if len(found) == 0 {
		return workspaceToolText("No symbols found."), nil
	}

	lines := splitSourceLines(content)
	rendered := make([]string, 0, len(found)+1)
	truncated := false
	for index, sym := range found {
		if index >= maxListedSymbols {
			truncated = true
			break
		}
		rendered = append(rendered, formatSymbolOutlineEntry(sym, lines))
	}
	if truncated {
		rendered = append(rendered, fmt.Sprintf(
			"Note: %d of %d symbols shown. Use repository_search to search the rest.",
			maxListedSymbols, len(found),
		))
	}
	return workspaceToolText(strings.Join(rendered, "\n")), nil
}

// formatSymbolOutlineEntry keeps the legacy "%4d | <source line>" shape so
// existing prompts and expectations still hold, and appends the span only when
// the declaration covers more than one line — that end line is the whole point
// of the tree-sitter upgrade.
func formatSymbolOutlineEntry(sym symbols.Symbol, lines []string) string {
	signature := ""
	if sym.Line-1 >= 0 && sym.Line-1 < len(lines) {
		signature = strings.TrimRight(lines[sym.Line-1], " \t\r")
	}
	signature = truncateReadRunes(strings.TrimSpace(signature), maxSymbolSignatureRunes)
	entry := fmt.Sprintf("%4d | %s", sym.Line, signature)
	if sym.EndLine > sym.Line {
		return fmt.Sprintf("%s  [%s %s, lines %d-%d]", entry, sym.Kind, sym.Name, sym.Line, sym.EndLine)
	}
	return fmt.Sprintf("%s  [%s %s]", entry, sym.Kind, sym.Name)
}

// legacyListSymbols is the original line-oriented scan, preserved verbatim in
// behaviour for file types with no grammar linked in.
func legacyListSymbols(path string, content []byte) (json.RawMessage, error) {
	ext := strings.ToLower(filepath.Ext(path))
	var patterns []*regexp.Regexp
	for _, sp := range workspaceSymbolPatterns {
		for _, candidate := range sp.exts {
			if candidate == ext {
				patterns = sp.patterns
				break
			}
		}
		if patterns != nil {
			break
		}
	}
	if patterns == nil {
		return nil, fmt.Errorf(
			"unsupported file type: %s (supported: %s)",
			ext, strings.Join(supportedSymbolExtensions(), ", "),
		)
	}
	var out []string
	for index, line := range splitSourceLines(content) {
		for _, pattern := range patterns {
			if pattern.MatchString(line) {
				out = append(out, fmt.Sprintf("%4d | %s", index+1, line))
				break
			}
		}
	}
	if len(out) == 0 {
		return workspaceToolText("No symbols found."), nil
	}
	return workspaceToolText(strings.Join(out, "\n")), nil
}

func (p *workspaceToolPack) readSymbol(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Path             string `json:"path"`
		Symbol           string `json:"symbol"`
		Kind             string `json:"kind"`
		IncludeDocstring *bool  `json:"include_docstring"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
	}
	symbolName := strings.TrimSpace(params.Symbol)
	if symbolName == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	includeDocstring := params.IncludeDocstring == nil || *params.IncludeDocstring

	root, err := requireWorkspaceRootForRepository(callCtx, "read_symbol", params.repoSelector())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.Path) == "" {
		hits, _, locateErr := p.locateDeclarations(ctx, root, symbolName)
		if locateErr != nil {
			return nil, locateErr
		}
		kind := strings.ToLower(strings.TrimSpace(params.Kind))
		if kind != "" {
			filtered := hits[:0:0]
			for _, hit := range hits {
				if string(hit.Kind) == kind {
					filtered = append(filtered, hit)
				}
			}
			hits = filtered
		}
		if len(hits) == 0 {
			return workspaceToolText(fmt.Sprintf("No declaration of %q found.", symbolName)), nil
		}
		if len(hits) > 1 {
			candidates := make([]symbolReadCandidate, 0, min(len(hits), maxSymbolMatchesReported))
			for _, hit := range hits[:min(len(hits), maxSymbolMatchesReported)] {
				candidates = append(candidates, symbolReadCandidate{hit.RelPath, hit.Kind, hit.Line, hit.EndLine})
			}
			return ambiguousSymbolRead(symbolName, len(hits), candidates), nil
		}
		params.Path = hits[0].RelPath
	}
	if !symbols.Supported(params.Path) {
		return nil, fmt.Errorf(
			"read_symbol does not support %s files (supported: %s); use list_symbols or repository_search then read_files",
			strings.ToLower(filepath.Ext(params.Path)),
			strings.Join(supportedSymbolExtensions(), ", "),
		)
	}
	content, err := p.readWorkspaceSourceForSymbols(root, params.Path)
	if err != nil {
		return nil, err
	}
	found, err := symbols.Extract(params.Path, content)
	if err != nil {
		return nil, err
	}
	matches := symbols.Find(found, symbolName)
	if len(matches) == 0 {
		return nil, symbolNotFoundError(params.Path, symbolName, found)
	}
	if len(matches) > 1 {
		candidates := make([]symbolReadCandidate, 0, min(len(matches), maxSymbolMatchesReported))
		for _, match := range matches[:min(len(matches), maxSymbolMatchesReported)] {
			candidates = append(candidates, symbolReadCandidate{params.Path, match.Kind, match.Line, match.EndLine})
		}
		return ambiguousSymbolRead(symbolName, len(matches), candidates), nil
	}

	target := matches[0]
	startLine := target.Line
	if includeDocstring {
		startLine = expandToLeadingComments(splitSourceLines(content), params.Path, target.Line)
	}
	limitLines := target.EndLine - startLine + 1
	if limitLines < 1 {
		limitLines = 1
	}
	clampedByLimit := false
	if limitLines > maxReadFileLimitLines {
		limitLines = maxReadFileLimitLines
		clampedByLimit = true
	}

	window, err := p.readTextFileWindow(ctx, callCtx, params.repoSelector(), params.Path, startLine, limitLines, "read_symbol", workspaceReadContentBudget(callCtx))
	if err != nil {
		return nil, err
	}
	// A symbol clipped by the per-call line cap must still advertise where to
	// resume, exactly as read_file does.
	if clampedByLimit && !window.HasMore {
		window.HasMore = true
		window.NextOffsetLine = startLine + len(window.Lines)
	}
	p.recordFileReads(callCtx, window)

	var out strings.Builder
	out.WriteString(fmt.Sprintf("%s %s in %s (lines %d-%d)\n",
		target.Kind, target.Name, displayReadPath(params.Path, maxReadDisplayedPathRunes), target.Line, target.EndLine))
	out.WriteString(formatReadFileWindow(window))
	return workspaceToolText(strings.TrimSpace(out.String())), nil
}

// symbolNotFoundError names near misses so the model can retry without another
// discovery round trip.
func symbolNotFoundError(path, symbolName string, found []symbols.Symbol) error {
	display := displayReadPath(path, maxReadDisplayedPathRunes)
	if len(found) == 0 {
		return fmt.Errorf("no symbols found in %s; use repository_search to locate %q", display, symbolName)
	}
	lowered := strings.ToLower(symbolName)
	var near []string
	for _, sym := range found {
		name := strings.ToLower(sym.Name)
		if strings.Contains(name, lowered) || strings.Contains(lowered, name) {
			near = append(near, sym.Name)
		}
	}
	near = dedupeSymbolNames(near)
	if len(near) > 0 {
		return fmt.Errorf("symbol %q not found in %s; did you mean: %s?",
			symbolName, display, strings.Join(limitStrings(near, maxSymbolMatchesReported), ", "))
	}
	available := make([]string, 0, len(found))
	for _, sym := range found {
		available = append(available, sym.Name)
	}
	available = dedupeSymbolNames(available)
	return fmt.Errorf("symbol %q not found in %s; %d symbols available: %s",
		symbolName, display, len(available),
		strings.Join(limitStrings(available, maxSymbolMatchesReported), ", "))
}

type symbolReadCandidate struct {
	Path      string       `json:"path"`
	Kind      symbols.Kind `json:"kind"`
	StartLine int          `json:"start_line"`
	EndLine   int          `json:"end_line"`
}

func ambiguousSymbolRead(name string, count int, candidates []symbolReadCandidate) json.RawMessage {
	payload, _ := json.Marshal(map[string]interface{}{
		"symbol": name, "ambiguous": true, "count": count, "candidates": candidates,
		"truncated": count > len(candidates),
		"hint":      fmt.Sprintf("Use read_files with the chosen candidate's path and start_line, limit_lines up to %d, and follow next_start_line through end_line.", maxReadFileLimitLines),
	})
	return payload
}

// expandToLeadingComments walks backwards from a declaration over a contiguous
// comment block so the documentation arrives with the code it describes.
func expandToLeadingComments(lines []string, path string, declLine int) int {
	prefixes, ok := commentPrefixes[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return declLine
	}
	start := declLine
	for candidate := declLine - 1; candidate >= 1 && declLine-candidate <= maxSymbolDocstringLines; candidate-- {
		if candidate-1 >= len(lines) {
			break
		}
		trimmed := strings.TrimSpace(lines[candidate-1])
		if trimmed == "" {
			break
		}
		matched := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(trimmed, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			break
		}
		start = candidate
	}
	return start
}

// splitSourceLines splits on \n and tolerates CRLF, matching how the read path
// renders lines.
func splitSourceLines(content []byte) []string {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	return strings.Split(text, "\n")
}

func supportedSymbolExtensions() []string {
	return symbols.SupportedExtensions()
}

func dedupeSymbolNames(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, value := range in {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func limitStrings(in []string, limit int) []string {
	if len(in) <= limit {
		return in
	}
	out := append([]string(nil), in[:limit]...)
	return append(out, fmt.Sprintf("… and %d more", len(in)-limit))
}
