package symbols

import (
	"testing"
)

const callFixtureGo = `
package sample

func helper() int { return 1 }

func Outer() int {
	total := helper()
	total += inner()
	return total
}

func inner() int {
	return helper()
}

func (c *Config) Method() int {
	return helper()
}
`

func TestExtractReferencesAttributesCallers(t *testing.T) {
	refs, err := ExtractReferences("sample.go", source(callFixtureGo))
	if err != nil {
		t.Fatalf("ExtractReferences: %v", err)
	}
	helperCalls := FindReferences(refs, "helper")
	if len(helperCalls) != 3 {
		t.Fatalf("expected 3 calls to helper, got %d: %+v", len(helperCalls), helperCalls)
	}
	callers := map[string]bool{}
	for _, ref := range helperCalls {
		callers[ref.Caller] = true
	}
	for _, want := range []string{"Outer", "inner", "Method"} {
		if !callers[want] {
			t.Errorf("call from %q not attributed; got callers %v", want, callers)
		}
	}
}

func TestExtractReferencesIgnoresDeclarationItself(t *testing.T) {
	refs, err := ExtractReferences("sample.go", source(callFixtureGo))
	if err != nil {
		t.Fatalf("ExtractReferences: %v", err)
	}
	// `func helper() int` is a declaration, not a call, so line 3 must not
	// appear among helper's references.
	for _, ref := range FindReferences(refs, "helper") {
		if ref.Line == 3 {
			t.Errorf("declaration line reported as a call site: %+v", ref)
		}
	}
}

func TestReferencesWithinSpanAnswersCallees(t *testing.T) {
	content := source(callFixtureGo)
	syms, err := Extract("sample.go", content)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	refs, err := ExtractReferences("sample.go", content)
	if err != nil {
		t.Fatalf("ExtractReferences: %v", err)
	}
	outer := Find(syms, "Outer")
	if len(outer) != 1 {
		t.Fatalf("expected one Outer declaration, got %d", len(outer))
	}
	within := ReferencesWithin(refs, outer[0].Line, outer[0].EndLine)
	names := map[string]bool{}
	for _, ref := range within {
		names[ref.Name] = true
	}
	if !names["helper"] || !names["inner"] {
		t.Errorf("callees of Outer = %v, want helper and inner", names)
	}
	// A call in a sibling function must not be attributed to Outer.
	if len(within) != 2 {
		t.Errorf("expected exactly 2 callees inside Outer, got %d: %+v", len(within), within)
	}
}

func TestExtractReferencesMethodCalls(t *testing.T) {
	const fixture = `
package sample

func Run() {
	client.Do(req)
	fmt.Println("x")
}
`
	refs, err := ExtractReferences("sample.go", source(fixture))
	if err != nil {
		t.Fatalf("ExtractReferences: %v", err)
	}
	names := map[string]bool{}
	for _, ref := range refs {
		names[ref.Name] = true
	}
	if !names["Do"] || !names["Println"] {
		t.Errorf("selector calls not captured: %v", names)
	}
}

func TestExtractReferencesAcrossLanguages(t *testing.T) {
	cases := []struct {
		path    string
		content string
		want    string
	}{
		{"sample.py", "def run():\n    helper()\n", "helper"},
		{"sample.ts", "export function run(): void {\n  helper();\n}\n", "helper"},
		{"sample.js", "export function run() {\n  helper();\n}\n", "helper"},
		{"sample.rs", "fn run() {\n    helper();\n}\n", "helper"},
		{"Sample.java", "class C {\n  void run() {\n    helper();\n  }\n}\n", "helper"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			refs, err := ExtractReferences(tc.path, []byte(tc.content))
			if err != nil {
				t.Fatalf("ExtractReferences: %v", err)
			}
			if len(FindReferences(refs, tc.want)) == 0 {
				t.Errorf("no reference to %q found; got %+v", tc.want, refs)
			}
		})
	}
}

// Adding reference patterns to the shared query must not change what Extract
// reports.
func TestReferenceQueriesDoNotDisturbDefinitions(t *testing.T) {
	syms, err := Extract("sample.go", source(callFixtureGo))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"helper", "Outer", "inner", "Method"} {
		if len(Find(syms, want)) == 0 {
			t.Errorf("definition %q lost after adding reference patterns", want)
		}
	}
	for _, sym := range syms {
		if sym.Kind != KindFunction && sym.Kind != KindMethod {
			t.Errorf("unexpected kind %q for %q — a reference capture may have leaked into definitions",
				sym.Kind, sym.Name)
		}
	}
}

// A React component is "called" as a JSX element, not a call expression.
// Without JSX patterns find_callers reports zero callers for every component.
func TestExtractReferencesCapturesJSXUsage(t *testing.T) {
	const fixture = `
export function Page() {
  return (
    <div>
      <Dialog open={true} />
      <Panel>
        <Inner />
      </Panel>
    </div>
  );
}
`
	for _, path := range []string{"page.tsx", "page.jsx"} {
		refs, err := ExtractReferences(path, source(fixture))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, want := range []string{"Dialog", "Panel", "Inner"} {
			if len(FindReferences(refs, want)) == 0 {
				t.Errorf("%s: JSX usage of %q not captured", path, want)
			}
		}
		for _, ref := range FindReferences(refs, "Dialog") {
			if ref.Caller != "Page" {
				t.Errorf("%s: JSX usage attributed to %q, want Page", path, ref.Caller)
			}
		}
	}
}

// Hooks and refs inside a component assigned to a const must not appear in the
// outline; the component binding is a scope even though its kind is "variable".
func TestComponentBodyLocalsAreNotOutlined(t *testing.T) {
	const fixture = `
export const ImageAnnotator = forwardRef(function ImageAnnotator(props) {
  const containerRef = useRef(null);
  const stageRef = useRef(null);
  const [scale, setScale] = useState(1);
  return null;
});

export const OTHER_CONSTANT = 5;
`
	syms, err := Extract("annotator.tsx", source(fixture))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, leaked := range []string{"containerRef", "stageRef"} {
		if len(Find(syms, leaked)) != 0 {
			t.Errorf("component-local %q leaked into the outline", leaked)
		}
	}
	if len(Find(syms, "ImageAnnotator")) == 0 {
		t.Error("the component itself must still be reported")
	}
	if len(Find(syms, "OTHER_CONSTANT")) == 0 {
		t.Error("a top-level constant must still be reported")
	}
}
