package symbols

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Reference is one call site.
//
// Caller is the declaration that encloses the call, which is what makes a
// caller list readable: "who calls this" is nearly useless as a list of bare
// line numbers.
type Reference struct {
	Name     string
	Line     int
	Caller   string
	CallerAt int
	Language string
}

// ExtractReferences returns the call sites in content, each attributed to the
// declaration that encloses it.
//
// Resolution is lexical, not semantic: a call to `Close` is reported whether it
// lands on an io.Closer or a database handle. Narrowing that would require type
// resolution, which tree-sitter alone cannot provide — so callers must treat
// results as candidates, and the tool descriptions say so.
func ExtractReferences(path string, content []byte) ([]Reference, error) {
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

	// Definitions are collected from the same parse so call sites can be
	// attributed without a second pass.
	var defs []Symbol
	var refs []Reference
	for _, tag := range tags {
		name := strings.TrimSpace(tag.Name)
		if name == "" {
			continue
		}
		start := int(tag.Range.StartPoint.Row) + 1
		end := int(tag.Range.EndPoint.Row) + 1
		if end < start {
			end = start
		}
		switch {
		case strings.HasPrefix(tag.Kind, "definition."):
			kind, ok := definitionKind(tag.Kind)
			if !ok {
				continue
			}
			defs = append(defs, Symbol{Name: name, Kind: kind, Line: start, EndLine: end, Language: spec.name})
		case strings.HasPrefix(tag.Kind, "reference."):
			refs = append(refs, Reference{Name: name, Line: start, Language: spec.name})
		}
	}

	// Prefer the innermost enclosing declaration, and only ones that can
	// actually contain code.
	callable := defs[:0:0]
	for _, def := range defs {
		if def.Kind == KindFunction || def.Kind == KindMethod || def.Kind == KindConstructor {
			callable = append(callable, def)
		}
	}
	sort.SliceStable(callable, func(i, j int) bool {
		if callable[i].Line != callable[j].Line {
			return callable[i].Line < callable[j].Line
		}
		return callable[i].EndLine < callable[j].EndLine
	})

	for i := range refs {
		best := -1
		for index, def := range callable {
			if def.Line > refs[i].Line {
				break
			}
			if refs[i].Line <= def.EndLine {
				// Innermost wins: later entries with the same start are tighter,
				// and a tighter span always starts no earlier.
				if best < 0 || def.Line >= callable[best].Line {
					best = index
				}
			}
		}
		if best >= 0 {
			refs[i].Caller = callable[best].Name
			refs[i].CallerAt = callable[best].Line
		}
	}

	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Line < refs[j].Line })
	return refs, nil
}

// FindReferences returns the call sites of name.
func FindReferences(refs []Reference, name string) []Reference {
	target := strings.TrimSpace(name)
	if target == "" {
		return nil
	}
	out := refs[:0:0]
	for _, ref := range refs {
		if ref.Name == target {
			out = append(out, ref)
		}
	}
	return out
}

// ReferencesWithin returns the call sites falling inside a line span, used to
// answer "what does this function call".
func ReferencesWithin(refs []Reference, start, end int) []Reference {
	out := refs[:0:0]
	for _, ref := range refs {
		if ref.Line >= start && ref.Line <= end {
			out = append(out, ref)
		}
	}
	return out
}
