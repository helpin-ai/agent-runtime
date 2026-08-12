package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/symbols"
)

const (
	// findSymbolMaxMentionFiles bounds the file list handed to the second
	// ripgrep pass. It also keeps the argument vector well inside ARG_MAX.
	findSymbolMaxMentionFiles = 2000
	// findSymbolMaxParseFiles bounds how many candidate files are parsed. On a
	// 3000-file repository a distinctive name yields 2 candidates and a very
	// common one yields ~36, so this only binds on pathological inputs.
	findSymbolMaxParseFiles = 50
	// findSymbolMaxResults bounds reported declarations.
	findSymbolMaxResults = 40
	findSymbolTimeout    = 30 * time.Second
	// maxRipgrepErrorRunes bounds how much of ripgrep's stderr is echoed back.
	maxRipgrepErrorRunes = 400
)

// skipTally records candidate files that were never examined. Skips must be
// reported: without them "no declaration found in 22 candidate files" reads as
// a confident negative when it may mean nothing could be parsed at all.
type skipTally struct {
	mu         sync.Mutex
	tooLarge   int
	unreadable int
	unparsed   int
}

func (s *skipTally) add(tooLarge, unreadable, unparsed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tooLarge += tooLarge
	s.unreadable += unreadable
	s.unparsed += unparsed
}

// note renders a human-readable summary, or "" when nothing was skipped.
func (s *skipTally) note() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var reasons []string
	if s.tooLarge > 0 {
		reasons = append(reasons, fmt.Sprintf("%d too large to parse (over %d bytes)", s.tooLarge, symbols.MaxParseBytes))
	}
	if s.unreadable > 0 {
		reasons = append(reasons, fmt.Sprintf("%d unreadable or binary", s.unreadable))
	}
	if s.unparsed > 0 {
		reasons = append(reasons, fmt.Sprintf("%d could not be parsed", s.unparsed))
	}
	if len(reasons) == 0 {
		return ""
	}
	return fmt.Sprintf("Note: %d candidate file(s) were skipped (%s); a declaration in them would not appear above.",
		s.tooLarge+s.unreadable+s.unparsed, strings.Join(reasons, ", "))
}

// collapseWhitespace flattens multi-line subprocess output into one line so it
// cannot break the surrounding tool-result formatting.
func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// ripgrepFileList runs `rg -l` and returns workspace file paths.
//
// Searching for declarations instead of walking and parsing the repository is
// what makes cross-file lookup viable: parsing is ~1000x more expensive per
// file than rejecting it with ripgrep, and a whole-repo parse measured 15s and
// ~1GB of heap on a mid-sized repository.
func ripgrepFileList(ctx context.Context, pattern string, literal bool, searchRoot string, paths []string) ([]string, error) {
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		return nil, fmt.Errorf("ripgrep (rg) not found in PATH; symbol lookup requires it")
	}
	args := []string{"-l", "--color", "never"}
	if literal {
		args = append(args, "-F", "-w",
			"--type-add", "src:*.{go,ts,tsx,js,jsx,mjs,cjs,py,pyi,rs,java}", "-t", "src",
			"--glob", "!.git", "--glob", "!node_modules", "--glob", "!vendor",
			"--glob", "!dist", "--glob", "!__pycache__")
	}
	args = append(args, "--", pattern)
	if len(paths) > 0 {
		args = append(args, paths...)
	} else {
		args = append(args, searchRoot)
	}

	timeout, cancel := context.WithTimeout(ctx, findSymbolTimeout)
	defer cancel()
	out, err := exec.CommandContext(timeout, rgPath, args...).Output()
	if err != nil {
		// rg exits 1 when nothing matched, which is a normal empty result.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		// Check the deadline before reporting stderr: a killed process carries
		// no useful diagnostic, and "timed out" is the actionable message.
		if timeout.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("ripgrep timed out after %s", findSymbolTimeout)
		}
		// rg writes the actual reason to stderr (bad pattern, unreadable path,
		// too many open files). Without this the caller only sees "exit status
		// 2", which is not enough to act on.
		if exitErr, ok := err.(*exec.ExitError); ok {
			if detail := strings.TrimSpace(string(exitErr.Stderr)); detail != "" {
				return nil, fmt.Errorf("ripgrep error: %s",
					truncateReadRunes(collapseWhitespace(detail), maxRipgrepErrorRunes))
			}
		}
		return nil, fmt.Errorf("ripgrep error: %w", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

type findSymbolHit struct {
	symbols.Symbol
	RelPath string
}

func (p *workspaceToolPack) findSymbol(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(params.Name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "find_symbol", params.repoSelector())
	if err != nil {
		return nil, err
	}
	kindFilter := strings.ToLower(strings.TrimSpace(params.Kind))

	// Stage 1: a literal word search, which ripgrep optimises hard (~40ms on a
	// 24MB repository) and which rejects the overwhelming majority of files.
	mentioned, err := ripgrepFileList(ctx, name, true, root, nil)
	if err != nil {
		return nil, err
	}
	mentioned = filterSupportedSources(mentioned)
	if len(mentioned) == 0 {
		return workspaceToolText(fmt.Sprintf("No files mention %q.", name)), nil
	}
	mentionTruncated := false
	if len(mentioned) > findSymbolMaxMentionFiles {
		mentioned = mentioned[:findSymbolMaxMentionFiles]
		mentionTruncated = true
	}

	// Stage 2: apply the declaration-shaped pattern to just those files, again
	// in ripgrep rather than in Go — Go's regexp engine measured 4x slower here.
	candidates, err := ripgrepFileList(ctx, symbols.DeclarationPattern(name), false, root, mentioned)
	if err != nil {
		return nil, err
	}
	candidates = filterSupportedSources(candidates)
	if len(candidates) == 0 {
		return workspaceToolText(fmt.Sprintf(
			"%q is mentioned in %d file(s) but declared in none of them; it is most likely imported from elsewhere.",
			name, len(mentioned))), nil
	}
	parseTruncated := false
	if len(candidates) > findSymbolMaxParseFiles {
		candidates = candidates[:findSymbolMaxParseFiles]
		parseTruncated = true
	}

	skips := &skipTally{}
	hits := parseCandidatesForSymbol(ctx, root, candidates, name, skips)
	if kindFilter != "" {
		filtered := hits[:0:0]
		for _, hit := range hits {
			if string(hit.Kind) == kindFilter {
				filtered = append(filtered, hit)
			}
		}
		hits = filtered
	}
	if len(hits) == 0 {
		detail := ""
		if kindFilter != "" {
			detail = fmt.Sprintf(" of kind %q", kindFilter)
		}
		message := fmt.Sprintf("No declaration of %q%s found in %d candidate file(s).", name, detail, len(candidates))
		if note := skips.note(); note != "" {
			message += "\n" + note
		}
		return workspaceToolText(message), nil
	}

	rankFindSymbolHits(hits)
	resultTruncated := false
	if len(hits) > findSymbolMaxResults {
		hits = hits[:findSymbolMaxResults]
		resultTruncated = true
	}

	var out strings.Builder
	out.WriteString(fmt.Sprintf("%d declaration(s) of %q (searched %d file(s), parsed %d):\n",
		len(hits), name, len(mentioned), len(candidates)))
	for _, hit := range hits {
		if hit.EndLine > hit.Line {
			out.WriteString(fmt.Sprintf("  %s:%d  [%s, lines %d-%d]\n",
				hit.RelPath, hit.Line, hit.Kind, hit.Line, hit.EndLine))
			continue
		}
		out.WriteString(fmt.Sprintf("  %s:%d  [%s]\n", hit.RelPath, hit.Line, hit.Kind))
	}
	for _, note := range truncationNotes(mentionTruncated, parseTruncated, resultTruncated) {
		out.WriteString(note + "\n")
	}
	if note := skips.note(); note != "" {
		out.WriteString(note + "\n")
	}
	out.WriteString("Use read_symbol with the path and name to read one in full.")
	return workspaceToolText(strings.TrimSpace(out.String())), nil
}

// parseCandidatesForSymbol parses candidates concurrently. Parsing dominates
// cost once ripgrep has narrowed the set, and the extractor is concurrency-safe
// (TestExtractConcurrent covers it under -race). Measurements showed no gain
// past 4 workers: the walk is memory-bandwidth bound, not core bound.
func parseCandidatesForSymbol(ctx context.Context, root string, candidates []string, name string, skips *skipTally) []findSymbolHit {
	workers := runtime.GOMAXPROCS(0)
	if workers > 4 {
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}

	// Results are collected under a mutex rather than through a buffered
	// channel: a single file can contain more matches than any buffer sized
	// from the candidate count, and a full buffer would block workers forever
	// while the collector waits on WaitGroup.
	jobs := make(chan string)
	var (
		mu   sync.Mutex
		hits []findSymbolHit
	)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				content, err := os.ReadFile(path)
				if err != nil {
					skips.add(0, 1, 0)
					continue
				}
				if len(content) > symbols.MaxParseBytes {
					skips.add(1, 0, 0)
					continue
				}
				if isBinaryContent(content) {
					skips.add(0, 1, 0)
					continue
				}
				found, err := symbols.Extract(path, content)
				if err != nil {
					skips.add(0, 0, 1)
					continue
				}
				relPath := workspaceRelativePath(root, path)
				matches := symbols.Find(found, name)
				if len(matches) == 0 {
					continue
				}
				local := make([]findSymbolHit, 0, len(matches))
				for _, sym := range matches {
					local = append(local, findSymbolHit{Symbol: sym, RelPath: relPath})
				}
				mu.Lock()
				hits = append(hits, local...)
				mu.Unlock()
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, path := range candidates {
			select {
			case <-ctx.Done():
				return
			case jobs <- path:
			}
		}
	}()
	wg.Wait()
	return hits
}

// rankFindSymbolHits orders results the way a reader wants them: real code
// before tests and generated stubs, exported before unexported. These are
// semble's cheap ranking signals, which need no embeddings.
func rankFindSymbolHits(hits []findSymbolHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		left, right := sourceRank(hits[i].RelPath), sourceRank(hits[j].RelPath)
		if left != right {
			return left < right
		}
		leftExported, rightExported := isExportedName(hits[i].Name), isExportedName(hits[j].Name)
		if leftExported != rightExported {
			return leftExported
		}
		if hits[i].RelPath != hits[j].RelPath {
			return hits[i].RelPath < hits[j].RelPath
		}
		return hits[i].Line < hits[j].Line
	})
}

// sourceRank downranks files that are rarely the definition a reader wants.
func sourceRank(relPath string) int {
	lower := strings.ToLower(relPath)
	base := filepath.Base(lower)
	switch {
	case strings.HasSuffix(base, ".d.ts"):
		return 3
	case strings.Contains(lower, "/vendor/") || strings.Contains(lower, "/node_modules/") ||
		strings.Contains(lower, "/third_party/") || strings.Contains(lower, "generated"):
		return 3
	case strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.tsx") || strings.HasSuffix(base, ".spec.ts") ||
		strings.HasPrefix(base, "test_") || strings.Contains(lower, "/tests/") ||
		strings.Contains(lower, "/__tests__/"):
		return 2
	case strings.Contains(lower, "/examples/") || strings.Contains(lower, "/mocks/"):
		return 2
	default:
		return 0
	}
}

func isExportedName(name string) bool {
	if name == "" {
		return false
	}
	first := []rune(name)[0]
	return first >= 'A' && first <= 'Z'
}

func filterSupportedSources(paths []string) []string {
	out := paths[:0:0]
	for _, path := range paths {
		if symbols.Supported(path) {
			out = append(out, path)
		}
	}
	return out
}

// workspaceRelativePath renders an absolute result path relative to the
// workspace root so output matches what other workspace tools accept as input.
func workspaceRelativePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// locateDeclarations runs the two-stage narrowing and returns the parsed
// declarations of name, ranked. Shared by find_symbol and find_callees.
func (p *workspaceToolPack) locateDeclarations(ctx context.Context, root, name string) ([]findSymbolHit, int, error) {
	mentioned, err := ripgrepFileList(ctx, name, true, root, nil)
	if err != nil {
		return nil, 0, err
	}
	mentioned = filterSupportedSources(mentioned)
	if len(mentioned) == 0 {
		return nil, 0, nil
	}
	if len(mentioned) > findSymbolMaxMentionFiles {
		mentioned = mentioned[:findSymbolMaxMentionFiles]
	}
	candidates, err := ripgrepFileList(ctx, symbols.DeclarationPattern(name), false, root, mentioned)
	if err != nil {
		return nil, len(mentioned), err
	}
	candidates = filterSupportedSources(candidates)
	if len(candidates) > findSymbolMaxParseFiles {
		candidates = candidates[:findSymbolMaxParseFiles]
	}
	hits := parseCandidatesForSymbol(ctx, root, candidates, name, &skipTally{})
	rankFindSymbolHits(hits)
	return hits, len(mentioned), nil
}

type callerHit struct {
	RelPath string
	Line    int
	Caller  string
}

func (p *workspaceToolPack) findCallers(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Symbol string `json:"symbol"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(params.Symbol)
	if name == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "find_callers", params.repoSelector())
	if err != nil {
		return nil, err
	}

	mentioned, err := ripgrepFileList(ctx, name, true, root, nil)
	if err != nil {
		return nil, err
	}
	mentioned = filterSupportedSources(mentioned)
	if len(mentioned) == 0 {
		return workspaceToolText(fmt.Sprintf("No files mention %q.", name)), nil
	}
	if len(mentioned) > findSymbolMaxMentionFiles {
		mentioned = mentioned[:findSymbolMaxMentionFiles]
	}
	candidates, err := ripgrepFileList(ctx, symbols.ReferencePattern(name), false, root, mentioned)
	if err != nil {
		return nil, err
	}
	candidates = filterSupportedSources(candidates)
	if len(candidates) == 0 {
		return workspaceToolText(fmt.Sprintf("No call sites of %q found.", name)), nil
	}
	parseTruncated := false
	if len(candidates) > findSymbolMaxParseFiles {
		candidates = candidates[:findSymbolMaxParseFiles]
		parseTruncated = true
	}

	skips := &skipTally{}
	hits := parseCandidatesForCallers(ctx, root, candidates, name, skips)
	if len(hits) == 0 {
		message := fmt.Sprintf("No call sites of %q found in %d candidate file(s).", name, len(candidates))
		if note := skips.note(); note != "" {
			message += "\n" + note
		}
		return workspaceToolText(message), nil
	}
	sort.SliceStable(hits, func(i, j int) bool {
		left, right := sourceRank(hits[i].RelPath), sourceRank(hits[j].RelPath)
		if left != right {
			return left < right
		}
		if hits[i].RelPath != hits[j].RelPath {
			return hits[i].RelPath < hits[j].RelPath
		}
		return hits[i].Line < hits[j].Line
	})
	resultTruncated := false
	if len(hits) > findSymbolMaxResults {
		hits = hits[:findSymbolMaxResults]
		resultTruncated = true
	}

	var out strings.Builder
	out.WriteString(fmt.Sprintf("%d call site(s) of %q (searched %d file(s), parsed %d):\n",
		len(hits), name, len(mentioned), len(candidates)))
	for _, hit := range hits {
		caller := hit.Caller
		if caller == "" {
			caller = "(top level)"
		}
		out.WriteString(fmt.Sprintf("  %s:%d  in %s\n", hit.RelPath, hit.Line, caller))
	}
	for _, note := range truncationNotes(false, parseTruncated, resultTruncated) {
		out.WriteString(note + "\n")
	}
	if note := skips.note(); note != "" {
		out.WriteString(note + "\n")
	}
	out.WriteString("Note: matching is by name, not by type, so calls to a different symbol with the same name may appear.")
	return workspaceToolText(strings.TrimSpace(out.String())), nil
}

func parseCandidatesForCallers(ctx context.Context, root string, candidates []string, name string, skips *skipTally) []callerHit {
	workers := runtime.GOMAXPROCS(0)
	if workers > 4 {
		workers = 4
	}
	if workers < 1 {
		workers = 1
	}
	// See parseCandidatesForSymbol: a buffered channel sized from the candidate
	// count deadlocks when one file holds more call sites than the buffer, which
	// is routine for a hot helper.
	jobs := make(chan string)
	var (
		mu   sync.Mutex
		hits []callerHit
	)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				content, err := os.ReadFile(path)
				if err != nil {
					skips.add(0, 1, 0)
					continue
				}
				if len(content) > symbols.MaxParseBytes {
					skips.add(1, 0, 0)
					continue
				}
				if isBinaryContent(content) {
					skips.add(0, 1, 0)
					continue
				}
				refs, err := symbols.ExtractReferences(path, content)
				if err != nil {
					skips.add(0, 0, 1)
					continue
				}
				matches := symbols.FindReferences(refs, name)
				if len(matches) == 0 {
					continue
				}
				relPath := workspaceRelativePath(root, path)
				local := make([]callerHit, 0, len(matches))
				for _, ref := range matches {
					local = append(local, callerHit{RelPath: relPath, Line: ref.Line, Caller: ref.Caller})
				}
				mu.Lock()
				hits = append(hits, local...)
				mu.Unlock()
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, path := range candidates {
			select {
			case <-ctx.Done():
				return
			case jobs <- path:
			}
		}
	}()
	wg.Wait()
	return hits
}

func (p *workspaceToolPack) findCallees(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		workspaceRepoSelector
		Symbol string `json:"symbol"`
		Path   string `json:"path"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(params.Symbol)
	if name == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	root, err := requireWorkspaceRootForRepository(callCtx, "find_callees", params.repoSelector())
	if err != nil {
		return nil, err
	}

	relPath := strings.TrimSpace(params.Path)
	var target symbols.Symbol
	if relPath == "" {
		hits, _, err := p.locateDeclarations(ctx, root, name)
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 {
			return workspaceToolText(fmt.Sprintf(
				"No declaration of %q found; pass path to disambiguate.", name)), nil
		}
		relPath = hits[0].RelPath
		target = hits[0].Symbol
	}

	content, err := p.readWorkspaceSourceForSymbols(root, relPath)
	if err != nil {
		return nil, err
	}
	if target.Name == "" {
		found, err := symbols.Extract(relPath, content)
		if err != nil {
			return nil, err
		}
		matches := symbols.Find(found, name)
		if len(matches) == 0 {
			return nil, symbolNotFoundError(relPath, name, found)
		}
		target = matches[0]
	}

	refs, err := symbols.ExtractReferences(relPath, content)
	if err != nil {
		return nil, err
	}
	within := symbols.ReferencesWithin(refs, target.Line, target.EndLine)
	if len(within) == 0 {
		return workspaceToolText(fmt.Sprintf(
			"%s %s (%s:%d-%d) makes no calls.", target.Kind, target.Name, relPath, target.Line, target.EndLine)), nil
	}

	// Collapse repeated calls to the same name; the first line is enough to
	// navigate, and a loop calling one helper ten times is one fact.
	type calleeEntry struct {
		name  string
		line  int
		count int
	}
	order := make([]string, 0, len(within))
	seen := make(map[string]*calleeEntry, len(within))
	for _, ref := range within {
		if entry, ok := seen[ref.Name]; ok {
			entry.count++
			continue
		}
		seen[ref.Name] = &calleeEntry{name: ref.Name, line: ref.Line, count: 1}
		order = append(order, ref.Name)
	}

	var out strings.Builder
	out.WriteString(fmt.Sprintf("%s %s at %s:%d-%d calls %d distinct symbol(s):\n",
		target.Kind, target.Name, relPath, target.Line, target.EndLine, len(order)))
	for index, key := range order {
		if index >= findSymbolMaxResults {
			out.WriteString(fmt.Sprintf("Note: results truncated to %d callees.\n", findSymbolMaxResults))
			break
		}
		entry := seen[key]
		if entry.count > 1 {
			out.WriteString(fmt.Sprintf("  %s  (first at line %d, %d calls)\n", entry.name, entry.line, entry.count))
			continue
		}
		out.WriteString(fmt.Sprintf("  %s  (line %d)\n", entry.name, entry.line))
	}
	out.WriteString("Note: matching is by name, not by type. Use find_symbol to locate a callee's declaration.")
	return workspaceToolText(strings.TrimSpace(out.String())), nil
}

func truncationNotes(mention, parse, results bool) []string {
	var notes []string
	if mention {
		notes = append(notes, fmt.Sprintf(
			"Note: more than %d files mention this name; only the first %d were considered.",
			findSymbolMaxMentionFiles, findSymbolMaxMentionFiles))
	}
	if parse {
		notes = append(notes, fmt.Sprintf(
			"Note: more than %d files declare this name; only the first %d were parsed.",
			findSymbolMaxParseFiles, findSymbolMaxParseFiles))
	}
	if results {
		notes = append(notes, fmt.Sprintf(
			"Note: results truncated to %d declarations.", findSymbolMaxResults))
	}
	return notes
}
