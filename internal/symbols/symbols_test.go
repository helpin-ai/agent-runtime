package symbols

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// source strips the leading newline of a raw literal so line 1 of the fixture
// is the first visible line, keeping asserted line numbers readable.
func source(text string) []byte {
	return []byte(strings.TrimPrefix(text, "\n"))
}

func extract(t *testing.T, path, text string) []Symbol {
	t.Helper()
	syms, err := Extract(path, source(text))
	if err != nil {
		t.Fatalf("Extract(%s): %v", path, err)
	}
	return syms
}

func find(t *testing.T, syms []Symbol, name string) Symbol {
	t.Helper()
	matches := Find(syms, name)
	if len(matches) == 0 {
		t.Fatalf("symbol %q not found; got %v", name, names(syms))
	}
	return matches[0]
}

func names(syms []Symbol) []string {
	out := make([]string, 0, len(syms))
	for _, sym := range syms {
		out = append(out, string(sym.Kind)+":"+sym.Name)
	}
	return out
}

func assertSpan(t *testing.T, sym Symbol, kind Kind, start, end int) {
	t.Helper()
	if sym.Kind != kind {
		t.Errorf("%s: kind = %q, want %q", sym.Name, sym.Kind, kind)
	}
	if sym.Line != start || sym.EndLine != end {
		t.Errorf("%s: span = %d..%d, want %d..%d", sym.Name, sym.Line, sym.EndLine, start, end)
	}
}

const goFixture = `
package sample

import "fmt"

type Config struct {
	Name string
}

type Runner interface {
	Run() error
}

type Alias = Config

const DefaultName = "runner"

const (
	ModeFast = "fast"
	ModeSlow = "slow"
)

var GlobalCount int

func New(name string) *Config {
	local := name
	const localConst = "nope"
	var localVar int
	helper := func() int { return localVar }
	fmt.Println(local, localConst, helper())
	return &Config{Name: name}
}

func (c *Config) Describe() string {
	return c.Name
}
`

func TestExtractGoSpansAndKinds(t *testing.T) {
	syms := extract(t, "sample.go", goFixture)

	assertSpan(t, find(t, syms, "Config"), KindType, 5, 7)
	assertSpan(t, find(t, syms, "Runner"), KindInterface, 9, 11)
	assertSpan(t, find(t, syms, "Alias"), KindType, 13, 13)
	assertSpan(t, find(t, syms, "DefaultName"), KindConstant, 15, 15)
	assertSpan(t, find(t, syms, "ModeFast"), KindConstant, 18, 18)
	assertSpan(t, find(t, syms, "GlobalCount"), KindVariable, 22, 22)
	assertSpan(t, find(t, syms, "New"), KindFunction, 24, 31)
	assertSpan(t, find(t, syms, "Describe"), KindMethod, 33, 35)

	for _, local := range []string{"localConst", "localVar", "local", "helper"} {
		if got := Find(syms, local); len(got) != 0 {
			t.Errorf("function-local %q leaked into the outline: %+v", local, got)
		}
	}
}

// Go's grammar reports an interface type_spec, but the tags query classifies
// every type_spec as a type. Guard the classification we actually ship rather
// than assuming the grammar distinguishes them.
func TestExtractGoInterfaceClassification(t *testing.T) {
	syms := extract(t, "sample.go", goFixture)
	sym := find(t, syms, "Runner")
	if sym.Kind != KindInterface && sym.Kind != KindType {
		t.Fatalf("Runner kind = %q, want interface or type", sym.Kind)
	}
}

const typescriptFixture = `
export interface Options {
  name: string;
}

export type Handler = (value: string) => void;

export enum Mode {
  Fast = "fast",
}

export class Service {
  private cache: string[] = [];

  public run(value: string): void {
    const inner = value;
    console.log(inner);
  }
}

export function build(name: string): Service {
  const local = name;
  return new Service();
}

export const factory = (name: string) => {
  return new Service();
};
`

func TestExtractTypeScript(t *testing.T) {
	syms := extract(t, "sample.ts", typescriptFixture)

	assertSpan(t, find(t, syms, "Options"), KindInterface, 1, 3)
	assertSpan(t, find(t, syms, "Handler"), KindType, 5, 5)
	assertSpan(t, find(t, syms, "Mode"), KindType, 7, 9)
	assertSpan(t, find(t, syms, "Service"), KindClass, 11, 18)
	assertSpan(t, find(t, syms, "run"), KindMethod, 14, 17)
	assertSpan(t, find(t, syms, "build"), KindFunction, 20, 23)

	// The dominant TS export style: an arrow function bound to a const.
	if got := Find(syms, "factory"); len(got) == 0 {
		t.Errorf("exported const arrow function missing; got %v", names(syms))
	}
	// Class fields are members, not function locals, so they stay.
	if got := Find(syms, "cache"); len(got) == 0 {
		t.Errorf("class field missing; got %v", names(syms))
	}
	if got := Find(syms, "inner"); len(got) != 0 {
		t.Errorf("method-local const leaked into the outline: %+v", got)
	}
	if got := Find(syms, "local"); len(got) != 0 {
		t.Errorf("function-local const leaked into the outline: %+v", got)
	}
}

func TestExtractTSXUsesTsxGrammar(t *testing.T) {
	const fixture = `
export function Panel(): JSX.Element {
  return <div className="panel">hi</div>;
}
`
	syms := extract(t, "panel.tsx", fixture)
	assertSpan(t, find(t, syms, "Panel"), KindFunction, 1, 3)
}

const pythonFixture = `
import functools

CONSTANT = 1


class Service:
    def run(self, value):
        local = value
        return local


@functools.cache
def build(name):
    return Service()


async def fetch(url):
    return url
`

func TestExtractPython(t *testing.T) {
	syms := extract(t, "sample.py", pythonFixture)

	assertSpan(t, find(t, syms, "Service"), KindClass, 6, 9)
	assertSpan(t, find(t, syms, "run"), KindMethod, 7, 9)
	assertSpan(t, find(t, syms, "fetch"), KindFunction, 17, 18)

	// A decorated definition must report the decorator line as its start, or a
	// caller reading the span sees a function stripped of its decorators.
	build := find(t, syms, "build")
	if build.Line != 12 {
		t.Errorf("decorated build starts at %d, want 12 (the decorator line)", build.Line)
	}
}

const rustFixture = `
pub mod helpers;

pub const LIMIT: usize = 4;

pub struct Config {
    pub name: String,
}

pub enum Mode {
    Fast,
}

pub trait Runner {
    fn run(&self) -> bool;
}

impl Config {
    pub fn new() -> Self {
        let local = 1;
        Config { name: String::new() }
    }
}

fn helper() -> bool {
    true
}
`

func TestExtractRust(t *testing.T) {
	syms := extract(t, "sample.rs", rustFixture)

	find(t, syms, "helpers")
	assertSpan(t, find(t, syms, "LIMIT"), KindConstant, 3, 3)
	assertSpan(t, find(t, syms, "Config"), KindType, 5, 7)
	assertSpan(t, find(t, syms, "Mode"), KindType, 9, 11)
	find(t, syms, "Runner")
	find(t, syms, "new")
	assertSpan(t, find(t, syms, "helper"), KindFunction, 24, 26)

	if got := Find(syms, "local"); len(got) != 0 {
		t.Errorf("function-local binding leaked: %+v", got)
	}
}

const javaFixture = `
package sample;

public class Service {
    private final String name;

    public Service(String name) {
        this.name = name;
    }

    public String describe() {
        String local = name;
        return local;
    }
}

interface Runner {
    void run();
}
`

func TestExtractJava(t *testing.T) {
	syms := extract(t, "Sample.java", javaFixture)

	assertSpan(t, find(t, syms, "Service"), KindClass, 3, 14)
	find(t, syms, "describe")
	find(t, syms, "Runner")
	if got := Find(syms, "name"); len(got) == 0 {
		t.Errorf("field declaration missing; got %v", names(syms))
	}
}

const javascriptFixture = `
export class Service {
  run(value) {
    const inner = value;
    return inner;
  }
}

export function build(name) {
  return new Service();
}

export const factory = () => new Service();
`

func TestExtractJavaScript(t *testing.T) {
	syms := extract(t, "sample.js", javascriptFixture)

	assertSpan(t, find(t, syms, "Service"), KindClass, 1, 6)
	find(t, syms, "run")
	assertSpan(t, find(t, syms, "build"), KindFunction, 8, 10)
	find(t, syms, "factory")
	if got := Find(syms, "inner"); len(got) != 0 {
		t.Errorf("method-local const leaked: %+v", got)
	}
}

// Every configured grammar must be linked in and its query must compile. This
// is the guard against the Dockerfile's grammar_subset tags drifting out of
// sync with extensionLanguages.
func TestAllConfiguredLanguagesUsable(t *testing.T) {
	snippets := map[string]string{
		".go":   "package p\nfunc F() {}\n",
		".ts":   "export function f(): void {}\n",
		".tsx":  "export function f(): JSX.Element { return <a/>; }\n",
		".js":   "export function f() {}\n",
		".jsx":  "export function f() { return <a/>; }\n",
		".mjs":  "export function f() {}\n",
		".cjs":  "function f() {}\n",
		".py":   "def f():\n    pass\n",
		".pyi":  "def f() -> None: ...\n",
		".rs":   "fn f() {}\n",
		".java": "class C { void f() {} }\n",
	}
	for ext, snippet := range snippets {
		t.Run(ext, func(t *testing.T) {
			if !Supported("x" + ext) {
				t.Fatalf("Supported(x%s) = false", ext)
			}
			syms, err := Extract("x"+ext, []byte(snippet))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(syms) == 0 {
				t.Fatalf("no symbols extracted from %s snippet", ext)
			}
		})
	}
}

func TestUnsupportedExtension(t *testing.T) {
	if Supported("notes.md") {
		t.Fatal("Supported(notes.md) = true")
	}
	_, err := Extract("notes.md", []byte("# hi\n"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestTooLargeRejectedBeforeParsing(t *testing.T) {
	oversized := make([]byte, MaxParseBytes+1)
	for i := range oversized {
		oversized[i] = 'a'
	}
	_, err := Extract("big.go", oversized)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestEmptyAndCommentOnlyFiles(t *testing.T) {
	for name, content := range map[string]string{
		"empty":        "",
		"comment only": "// nothing here\n",
		"package only": "package p\n",
	} {
		syms, err := Extract("sample.go", []byte(content))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(syms) != 0 {
			t.Errorf("%s: expected no symbols, got %v", name, names(syms))
		}
	}
}

// Malformed source must not error: agents read files mid-edit all the time, and
// a partial parse is more useful than a failure.
func TestSyntaxErrorsStillYieldSymbols(t *testing.T) {
	const broken = `
package p

func Good() int {
	return 1
}

func Broken( {
`
	syms, err := Extract("sample.go", source(broken))
	if err != nil {
		t.Fatalf("Extract on malformed source: %v", err)
	}
	if len(Find(syms, "Good")) == 0 {
		t.Errorf("valid declaration lost on malformed file; got %v", names(syms))
	}
}

func TestFindPrefersExactMatch(t *testing.T) {
	syms := []Symbol{
		{Name: "Handler", Kind: KindType, Line: 1, EndLine: 2},
		{Name: "handler", Kind: KindFunction, Line: 4, EndLine: 6},
	}
	got := Find(syms, "handler")
	if len(got) != 1 || got[0].Kind != KindFunction {
		t.Fatalf("exact match not preferred: %+v", got)
	}
	// Only when nothing matches exactly does case folding apply.
	got = Find(syms, "HANDLER")
	if len(got) != 2 {
		t.Fatalf("case-insensitive fallback = %d results, want 2", len(got))
	}
	if len(Find(syms, "  ")) != 0 {
		t.Fatal("blank name should match nothing")
	}
}

func TestDuplicateNamesAllReported(t *testing.T) {
	const fixture = `
package p

func Dup() {}

type Dup struct{}
`
	syms := extract(t, "sample.go", fixture)
	if got := Find(syms, "Dup"); len(got) != 2 {
		t.Fatalf("Find returned %d matches, want 2: %+v", len(got), got)
	}
}

// Taggers are pooled and each owns a parser; concurrent Extract must be safe.
func TestExtractConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			syms, err := Extract("sample.go", source(goFixture))
			if err != nil {
				errs <- err
				return
			}
			if len(Find(syms, "New")) == 0 {
				errs <- errors.New("missing New")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Extract: %v", err)
	}
}

// legacyPatterns mirrors workspaceSymbolPatterns in
// internal/tools/workspace_tools.go. The contract is that the tree-sitter
// extractor never loses a declaration the regex scan used to report.
var legacyPatterns = map[string][]*regexp.Regexp{
	".go": {
		regexp.MustCompile(`^func\s`),
		regexp.MustCompile(`^type\s+\w+\s+(struct|interface)`),
		regexp.MustCompile(`^type\s+\w+\s`),
		regexp.MustCompile(`^var\s+\w+`),
		regexp.MustCompile(`^const\s+\w+`),
	},
	".ts": {
		regexp.MustCompile(`^export\s+(function|const|class|type|interface|enum)\s`),
		regexp.MustCompile(`^\s*(public|private|protected|async)\s+\w+\(`),
		regexp.MustCompile(`^function\s+\w+`),
	},
	".js": {
		regexp.MustCompile(`^export\s+(function|const|class)\s`),
		regexp.MustCompile(`^function\s+\w+`),
		regexp.MustCompile(`^class\s+\w+`),
	},
	".py": {
		regexp.MustCompile(`^def\s+\w+`),
		regexp.MustCompile(`^class\s+\w+`),
		regexp.MustCompile(`^async\s+def\s+\w+`),
	},
	".rs": {
		regexp.MustCompile(`^pub\s+(fn|struct|enum|trait|type|impl|mod)\s`),
		regexp.MustCompile(`^fn\s+`),
		regexp.MustCompile(`^struct\s+`),
		regexp.MustCompile(`^enum\s+`),
		regexp.MustCompile(`^trait\s+`),
		regexp.MustCompile(`^impl\s`),
	},
}

func TestNoRegressionAgainstLegacyRegexScan(t *testing.T) {
	cases := []struct {
		path    string
		content string
	}{
		{"sample.go", goFixture},
		{"sample.ts", typescriptFixture},
		{"sample.js", javascriptFixture},
		{"sample.py", pythonFixture},
		{"sample.rs", rustFixture},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			content := source(tc.content)
			syms, err := Extract(tc.path, content)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			covered := make(map[int]bool, len(syms))
			for _, sym := range syms {
				covered[sym.Line] = true
			}

			ext := tc.path[strings.LastIndex(tc.path, "."):]
			for i, line := range strings.Split(string(content), "\n") {
				lineNo := i + 1
				matched := false
				for _, re := range legacyPatterns[ext] {
					if re.MatchString(line) {
						matched = true
						break
					}
				}
				if !matched || covered[lineNo] {
					continue
				}
				// A legacy match with no symbol on that line is a regression
				// unless the declaration genuinely has no name to report.
				t.Errorf("regression: legacy scan reported line %d (%q) but no symbol covers it",
					lineNo, strings.TrimSpace(line))
			}
		})
	}
}
