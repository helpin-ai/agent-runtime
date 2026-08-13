package symbols

import (
	"regexp"
	"strings"
)

// DeclarationPattern builds a regex that matches lines plausibly *declaring*
// name, for use as a ripgrep file-level prefilter ahead of parsing.
//
// This is a filter, not an oracle. It decides which files are worth opening;
// Extract decides what is actually declared. The asymmetry matters:
//
//   - a false positive costs one wasted parse (~9ms)
//   - a false negative loses the declaration entirely
//
// so the pattern deliberately over-matches. Narrowing it to reduce parses is
// the wrong trade and will silently break lookups.
//
// Searching for declarations rather than mentions is what makes the whole
// approach viable: on a 3000-file repository, "Config" appears in 157 files but
// is declared in 2, and "useState" appears in 341 files and is declared in none.
func DeclarationPattern(name string) string {
	quoted := regexp.QuoteMeta(strings.TrimSpace(name))
	if quoted == "" {
		return ""
	}

	// Keyword-led declarations: Go, TS/JS, Python, Rust, Java, C#.
	// Optional generic/type parameters are tolerated between name and body.
	keyword := `(?:func|function|type|class|def|fn|interface|enum|struct|trait|record|impl|mod|module|namespace|object|val|proc)`

	alternatives := []string{
		// func Name, class Name, def Name, ...
		`\b` + keyword + `\s+` + quoted + `\b`,
		// Binding forms: const Name = ..., let Name: ..., Name := ...
		// Covers `export const factory = () => {}`, the dominant TS export style.
		`\b(?:const|let|var|static|pub|export|final)\s+` + quoted + `\b`,
		quoted + `\s*:?=`,
		// Class members and annotated/modified methods:
		//   public String describe(), async run(), private readonly x =
		`^\s*(?:@[\w.]+\s*)*(?:public|private|protected|internal|static|async|abstract|override|readonly|final|virtual)[\w\s<>\[\],.\*&]*\b` + quoted + `\s*[(<]`,
		// Bare member declarations inside a class/object body:
		//   run(value) {, Name: function() {, Name(...) =>
		`^\s*` + quoted + `\s*[(<]`,
		// Typed declarators, where a type precedes the name and no keyword
		// leads the line: `void run();`, `private final String name;`,
		// `private cache: string[] = []`. Requiring at least one
		// whitespace-terminated token before the name is what keeps this from
		// matching a qualified call like `pkg.Name(...)`, since the token
		// character class excludes `.` and `:`.
		`^\s*(?:@[\w.]+\s*)*(?:[\w<>\[\],\*&\?]+\s+)+` + quoted + `\s*[(;:=<]`,
		`^\s*` + quoted + `\s*:\s*(?:async\s+)?(?:function|\()`,
		// Go methods carry a receiver before the name: func (c *Config) Name(
		`\bfunc\s*\([^)]*\)\s*` + quoted + `\b`,
		// export default function Name / export default class Name
		`\bexport\s+default\s+(?:async\s+)?` + keyword + `?\s*` + quoted + `\b`,
		// Decorated Python/TS definitions where the decorator sits above; the
		// definition line itself still matches the keyword form, so this covers
		// only the `Name = ` assignment style used for decorated factories.
		`\b` + quoted + `\s*=\s*(?:async\s+)?(?:function|class|\(|<)`,
	}
	return "(?:" + strings.Join(alternatives, "|") + ")"
}

// ReferencePattern builds a regex matching plausible *call sites* of name, for
// narrowing ahead of call-graph extraction. Same over-match contract as
// DeclarationPattern.
func ReferencePattern(name string) string {
	quoted := regexp.QuoteMeta(strings.TrimSpace(name))
	if quoted == "" {
		return ""
	}
	// A call is the name followed by an open paren, optionally through a
	// selector (obj.Name(), pkg::Name(), self.Name()) or generics (Name[T]()).
	call := `(?:[\w.:>-]*\b)?` + quoted + `\s*(?:\[[^\]]*\]|<[^>]*>)?\s*\(`
	// JSX usage has no parentheses at all: `<Dialog {...props} />`. Omitting
	// this makes the prefilter reject every file that renders a component,
	// so find_callers would report zero callers for the whole React codebase
	// even though the extractor understands JSX.
	jsx := `<` + quoted + `[\s/>]`
	return `(?:` + call + `|` + jsx + `)`
}

// compiledDeclarationPattern is used by tests and by callers that want to check
// a single line without shelling out to ripgrep.
func compiledDeclarationPattern(name string) (*regexp.Regexp, error) {
	pattern := DeclarationPattern(name)
	if pattern == "" {
		return nil, nil
	}
	return regexp.Compile("(?m)" + pattern)
}

func compiledReferencePattern(name string) (*regexp.Regexp, error) {
	pattern := ReferencePattern(name)
	if pattern == "" {
		return nil, nil
	}
	return regexp.Compile("(?m)" + pattern)
}
