// Package symbols extracts declarations from source files using a pure-Go
// tree-sitter runtime.
//
// It exists so callers can resolve a symbol name to an exact line range. The
// regex-based scan it replaces could only report where a declaration starts,
// which forced callers to guess how far to read.
//
// The runtime is CGO-free on purpose: agent-runtime binaries are built with
// CGO_ENABLED=0 (see Dockerfile), which rules out the cgo tree-sitter bindings.
// Grammars are selected at build time via the grammar_subset build tags; a
// grammar that is not linked in degrades to ErrUnsupported rather than failing.
package symbols

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// ErrUnsupported means no grammar is linked in for the file's extension. It is
// not a failure: callers are expected to fall back to a coarser scan.
var ErrUnsupported = errors.New("no symbol grammar for this file type")

// MaxParseBytes bounds how much source is handed to the parser. Files above it
// return ErrTooLarge; the parser cost is superlinear on pathological inputs and
// a workspace can contain generated files of arbitrary size.
const MaxParseBytes = 2 << 20 // 2 MiB

// ErrTooLarge means the file exceeds MaxParseBytes.
var ErrTooLarge = errors.New("file too large to parse for symbols")

// Kind classifies a declaration. Values are stable and are surfaced to model
// callers, so they must stay lowercase and human-obvious.
type Kind string

const (
	KindFunction    Kind = "function"
	KindMethod      Kind = "method"
	KindType        Kind = "type"
	KindClass       Kind = "class"
	KindInterface   Kind = "interface"
	KindConstructor Kind = "constructor"
	KindConstant    Kind = "constant"
	KindVariable    Kind = "variable"
	KindModule      Kind = "module"
)

// Symbol is one declaration. Line and EndLine are 1-based and inclusive so they
// can be handed straight to a line-window reader.
type Symbol struct {
	Name     string
	Kind     Kind
	Line     int
	EndLine  int
	Language string
}

// bodyScoped kinds are only meaningful at file or type scope. Inside a function
// body they are locals, which are noise for an outline. The regex scan this
// replaces was anchored at column 0 and so never reported them; surfacing them
// now would be a silent behaviour change.
func (k Kind) bodyScoped() bool {
	return k == KindConstant || k == KindVariable
}

// encloses kinds establish a scope that makes nested declarations locals.
// Classes and interfaces are deliberately excluded: their members are wanted.
func (k Kind) encloses() bool {
	return k == KindFunction || k == KindMethod || k == KindConstructor
}

// Supported reports whether Extract can handle the file, without reading it.
func Supported(path string) bool {
	_, ok := lookupLanguage(path)
	return ok
}

// SupportedExtensions lists the file extensions Extract can handle, sorted.
// It is the single source of truth for the extension list quoted in tool
// descriptions and error messages.
func SupportedExtensions() []string {
	return sortedExtensions()
}

// Extract returns the declarations in content, ordered by start line then name.
//
// content must be the file's exact bytes; line numbers are derived from it.
func Extract(path string, content []byte) ([]Symbol, error) {
	if len(content) > MaxParseBytes {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrTooLarge, len(content), MaxParseBytes)
	}
	spec, ok := lookupLanguage(path)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, strings.ToLower(filepath.Ext(path)))
	}
	tagger, release, err := spec.acquire()
	if err != nil {
		return nil, err
	}
	defer release()

	tags := tagger.Tag(content)
	out := make([]Symbol, 0, len(tags))
	for _, tag := range tags {
		kind, ok := definitionKind(tag.Kind)
		if !ok {
			continue
		}
		name := strings.TrimSpace(tag.Name)
		if name == "" {
			continue
		}
		start := int(tag.Range.StartPoint.Row) + 1
		end := int(tag.Range.EndPoint.Row) + 1
		if end < start {
			end = start
		}
		out = append(out, Symbol{
			Name:     name,
			Kind:     kind,
			Line:     start,
			EndLine:  end,
			Language: spec.name,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		if out[i].EndLine != out[j].EndLine {
			// Outer declarations first so scope tracking sees them before members.
			return out[i].EndLine > out[j].EndLine
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		// More specific kind first so dedupe keeps it.
		return kindSpecificity(out[i].Kind) > kindSpecificity(out[j].Kind)
	})
	out = dedupe(out)
	promoteMethods(out)
	return dropFunctionLocals(out), nil
}

// kindSpecificity ranks kinds so that, when two queries match the same span,
// the more descriptive classification wins.
func kindSpecificity(kind Kind) int {
	switch kind {
	case KindConstructor:
		return 4
	case KindMethod, KindInterface, KindClass:
		return 3
	case KindFunction, KindType, KindModule:
		return 2
	default:
		return 1
	}
}

// dedupe collapses matches that describe the same declaration. Overlapping
// patterns are intentional (see goQuery), so this is a correctness requirement,
// not a tidy-up. Input must be sorted with the most specific kind first.
func dedupe(in []Symbol) []Symbol {
	type key struct {
		name    string
		line    int
		endLine int
	}
	seen := make(map[key]struct{}, len(in))
	out := in[:0:0]
	for _, sym := range in {
		id := key{sym.Name, sym.Line, sym.EndLine}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, sym)
	}
	return out
}

// promoteMethods reclassifies a function declared inside a class or interface
// as a method. Python and Rust have no distinct method node — both spell it
// function_definition / function_item — so without this the same construct is
// reported as "method" in Go, TypeScript and Java but "function" in Python.
// Input must be sorted by start line, outermost first.
func promoteMethods(syms []Symbol) {
	var scopes []Symbol
	for i, sym := range syms {
		for len(scopes) > 0 && sym.Line > scopes[len(scopes)-1].EndLine {
			scopes = scopes[:len(scopes)-1]
		}
		if sym.Kind == KindFunction && len(scopes) > 0 {
			syms[i].Kind = KindMethod
			sym = syms[i]
		}
		if sym.Kind == KindClass || sym.Kind == KindInterface {
			scopes = append(scopes, sym)
		}
	}
}

// dropFunctionLocals removes constants and variables declared inside a function
// body. Input must be sorted by start line, outermost first.
func dropFunctionLocals(in []Symbol) []Symbol {
	out := in[:0:0]
	var scopes []Symbol
	for _, sym := range in {
		for len(scopes) > 0 && sym.Line > scopes[len(scopes)-1].EndLine {
			scopes = scopes[:len(scopes)-1]
		}
		if sym.Kind.bodyScoped() && len(scopes) > 0 {
			continue
		}
		out = append(out, sym)
		// A multi-line binding is a function expression in all but name —
		// `const Component = forwardRef(function () { ... })` is the dominant
		// React idiom — so its body holds locals just like a function's does.
		// Without this every hook and ref inside a component leaks into the
		// outline.
		if sym.Kind.encloses() || (sym.Kind.bodyScoped() && sym.EndLine > sym.Line) {
			scopes = append(scopes, sym)
		}
	}
	return out
}

// Find returns declarations matching name, exact match first.
//
// Matching is case-sensitive; a case-insensitive fallback runs only when the
// exact match finds nothing, so callers get a useful result for a mistyped
// capital without ever shadowing a real match.
func Find(syms []Symbol, name string) []Symbol {
	target := strings.TrimSpace(name)
	if target == "" {
		return nil
	}
	var exact []Symbol
	for _, sym := range syms {
		if sym.Name == target {
			exact = append(exact, sym)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	var fold []Symbol
	for _, sym := range syms {
		if strings.EqualFold(sym.Name, target) {
			fold = append(fold, sym)
		}
	}
	return fold
}

// definitionKind maps a tags-query capture ("definition.method") to a Kind.
// reference.* captures are ignored here; they describe call sites, not
// declarations.
func definitionKind(capture string) (Kind, bool) {
	suffix, ok := strings.CutPrefix(capture, "definition.")
	if !ok {
		return "", false
	}
	switch Kind(suffix) {
	case KindFunction, KindMethod, KindType, KindClass, KindInterface,
		KindConstructor, KindConstant, KindVariable, KindModule:
		return Kind(suffix), true
	}
	// Unknown definition.* captures are real declarations from an inferred
	// query; keep them rather than silently dropping a symbol.
	return Kind(suffix), true
}

// languageSpec binds an extension group to a grammar and its tags query.
// Taggers are pooled because each one owns a parser and is not safe for
// concurrent use, and because compiling the query per call roughly doubled cost.
type languageSpec struct {
	name  string
	query string

	once sync.Once
	err  error
	lang *gts.Language
	pool sync.Pool
}

func (s *languageSpec) acquire() (*gts.Tagger, func(), error) {
	s.once.Do(func() {
		entry := grammars.DetectLanguageByName(s.name)
		if entry == nil || entry.Language == nil {
			s.err = fmt.Errorf("%w: grammar %q is not linked into this binary", ErrUnsupported, s.name)
			return
		}
		lang := entry.Language()
		if lang == nil {
			s.err = fmt.Errorf("%w: grammar %q failed to load", ErrUnsupported, s.name)
			return
		}
		query := s.query
		if strings.TrimSpace(query) == "" {
			// Fall back to the runtime's inferred query. Built-in grammars ship
			// an empty LangEntry.TagsQuery, so ResolveTagsQuery is the only
			// accessor that returns anything for them.
			query = grammars.ResolveTagsQuery(*entry)
		}
		if strings.TrimSpace(query) == "" {
			s.err = fmt.Errorf("%w: grammar %q has no tags query", ErrUnsupported, s.name)
			return
		}
		// Compile once up front so a malformed query fails here, not per call.
		if _, err := gts.NewTagger(lang, query); err != nil {
			s.err = fmt.Errorf("compile tags query for %q: %w", s.name, err)
			return
		}
		s.lang = lang
		s.query = query
		s.pool = sync.Pool{New: func() any {
			tagger, err := gts.NewTagger(s.lang, s.query)
			if err != nil {
				return err
			}
			return tagger
		}}
	})
	if s.err != nil {
		return nil, nil, s.err
	}
	switch value := s.pool.Get().(type) {
	case *gts.Tagger:
		return value, func() { s.pool.Put(value) }, nil
	case error:
		return nil, nil, fmt.Errorf("build tagger for %q: %w", s.name, value)
	default:
		return nil, nil, fmt.Errorf("build tagger for %q: unexpected pool value", s.name)
	}
}
