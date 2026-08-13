package symbols

import (
	"strings"
	"testing"
)

// The prefilter's contract: every declaration Extract can find must survive it.
// Extract is ground truth, so this is checked by construction rather than by a
// hand-written list of names that could drift.
func TestPrefilterRecallAgainstExtractor(t *testing.T) {
	fixtures := []struct {
		path    string
		content string
	}{
		{"sample.go", goFixture},
		{"sample.ts", typescriptFixture},
		{"sample.js", javascriptFixture},
		{"sample.py", pythonFixture},
		{"sample.rs", rustFixture},
		{"Sample.java", javaFixture},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.path, func(t *testing.T) {
			content := source(fixture.content)
			found, err := Extract(fixture.path, content)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(found) == 0 {
				t.Fatalf("fixture yielded no symbols")
			}
			for _, sym := range found {
				re, err := compiledDeclarationPattern(sym.Name)
				if err != nil {
					t.Fatalf("compile pattern for %q: %v", sym.Name, err)
				}
				if !re.Match(content) {
					t.Errorf("prefilter would skip the file declaring %s %q — the lookup would find nothing",
						sym.Kind, sym.Name)
				}
			}
		})
	}
}

// Declaration shapes that a naive `keyword NAME` pattern misses. Each is a real
// style in the languages we support, and each would be an invisible lookup
// failure rather than a visible error.
func TestPrefilterCoversAwkwardDeclarationShapes(t *testing.T) {
	cases := []struct {
		name    string
		symbol  string
		snippet string
	}{
		{"go method with pointer receiver", "Describe", "func (c *Config) Describe() string {"},
		{"go method with value receiver", "Describe", "func (c Config) Describe() string {"},
		{"go generic function", "Map", "func Map[T any, U any](in []T) []U {"},
		{"go grouped const", "ModeFast", "\tModeFast = \"fast\""},
		{"ts exported arrow const", "factory", "export const factory = (name: string) => {"},
		{"ts class method", "run", "  public run(value: string): void {"},
		{"ts bare class method", "run", "  run(value) {"},
		{"ts async class method", "load", "  async load(): Promise<void> {"},
		{"ts type alias", "Handler", "export type Handler = (value: string) => void;"},
		{"ts generic function", "identity", "export function identity<T>(value: T): T {"},
		{"ts abstract method", "execute", "  protected abstract execute(): void;"},
		{"js object literal method", "handler", "  handler: function () {"},
		{"js object literal arrow", "handler", "  handler: async () => {"},
		{"js export default function", "App", "export default function App() {"},
		{"js class field arrow", "onClick", "  onClick = () => {"},
		{"py decorated function", "build", "def build(name):"},
		{"py async def", "fetch", "async def fetch(url):"},
		{"py method", "run", "    def run(self, value):"},
		{"rust impl method", "new", "    pub fn new() -> Self {"},
		{"rust trait method signature", "run", "    fn run(&self) -> bool;"},
		{"rust struct", "Config", "pub struct Config {"},
		{"java annotated method", "describe", "    @Override\n    public String describe() {"},
		{"java generic method", "convert", "    public <T> T convert(Object raw) {"},
		{"java constructor", "Service", "    public Service(String name) {"},
		{"java field", "name", "    private final String name;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			re, err := compiledDeclarationPattern(tc.symbol)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if !re.MatchString(tc.snippet) {
				t.Errorf("prefilter misses %q in:\n%s", tc.symbol, tc.snippet)
			}
		})
	}
}

// The filter still has to filter. If it matched every file it would be a slow
// no-op, and the whole design would collapse back to parsing the repository.
func TestPrefilterRejectsFilesWithOnlyMentions(t *testing.T) {
	const consumer = `
package main

import "example.com/sample"

func main() {
	cfg := sample.NewConfig()
	sample.Describe(cfg)
	_ = sample.DefaultName
}
`
	for _, symbol := range []string{"NewConfig", "Describe", "DefaultName"} {
		re, err := compiledDeclarationPattern(symbol)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if re.MatchString(consumer) {
			t.Errorf("prefilter matched %q in a file that only calls it; the filter is too loose", symbol)
		}
	}
}

// The imported-symbol case that motivated skipping the whole-repo index: a name
// used everywhere and declared nowhere must not select the file.
func TestPrefilterIgnoresImportedIdentifiers(t *testing.T) {
	const component = `
import { useState, useEffect } from "react";

export function Panel() {
  const [open, setOpen] = useState(false);
  useEffect(() => setOpen(true), []);
  return open;
}
`
	re, err := compiledDeclarationPattern("useState")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if re.MatchString(component) {
		t.Error("prefilter selected a file that imports useState but never declares it")
	}
	// The component itself is declared here and must be found.
	re, err = compiledDeclarationPattern("Panel")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !re.MatchString(component) {
		t.Error("prefilter missed the exported component declaration")
	}
}

func TestPrefilterHandlesRegexMetacharacters(t *testing.T) {
	// A symbol name is attacker-influenced input; it must be quoted, not
	// interpolated, or a name like "a.*" would match everything.
	re, err := compiledDeclarationPattern("a.*b")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if re.MatchString("func aXXXb() {}") {
		t.Error("metacharacters in the symbol name were not escaped")
	}
	if !re.MatchString("func a.*b() {}") {
		t.Error("literal name should still match")
	}
}

func TestPrefilterBlankName(t *testing.T) {
	if got := DeclarationPattern("   "); got != "" {
		t.Errorf("blank name should yield no pattern, got %q", got)
	}
	if got := ReferencePattern(""); got != "" {
		t.Errorf("blank name should yield no reference pattern, got %q", got)
	}
}

func TestReferencePatternMatchesCallSites(t *testing.T) {
	for _, snippet := range []string{
		"result := Login(ctx, user)",
		"auth.Login(ctx)",
		"this.Login()",
		"obj->Login()",
		"crate::auth::Login()",
		"Login[T](value)",
		"await Login()",
	} {
		re, err := compileReferencePattern(t, "Login")
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if !re.MatchString(snippet) {
			t.Errorf("reference pattern missed call site: %s", snippet)
		}
	}
	re, _ := compileReferencePattern(t, "Login")
	if re.MatchString("// Login is documented here") {
		t.Error("reference pattern matched prose without a call")
	}
}

func compileReferencePattern(t *testing.T, name string) (interface {
	MatchString(string) bool
}, error) {
	t.Helper()
	return compiledReferencePattern(name)
}

func TestDeclarationPatternIsSingleLine(t *testing.T) {
	// ripgrep is line-oriented by default; a pattern relying on newlines would
	// silently fail there while passing an in-process test.
	pattern := DeclarationPattern("Name")
	if strings.Contains(pattern, `\n`) {
		t.Errorf("pattern must not depend on newlines: %s", pattern)
	}
}

// The prefilter must accept JSX usage or find_callers never opens the files
// that render a component, regardless of what the extractor can parse.
func TestReferencePatternMatchesJSXUsage(t *testing.T) {
	re, err := compiledReferencePattern("Dialog")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, snippet := range []string{
		"<Dialog {...props} />",
		"<Dialog open={true}>",
		"<Dialog/>",
		"  return <Dialog />;",
	} {
		if !re.MatchString(snippet) {
			t.Errorf("reference pattern missed JSX usage: %s", snippet)
		}
	}
	// A different component whose name merely starts the same must not match.
	if re.MatchString("<DialogFooter />") {
		t.Error("reference pattern matched a different component name")
	}
}
