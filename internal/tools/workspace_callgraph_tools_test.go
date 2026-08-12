package tools

import (
	"strings"
	"testing"
	"time"
)

func seedCallGraphWorkspace(t *testing.T, callCtx CallContext) {
	t.Helper()
	writeWorkspaceFixture(t, callCtx, "internal/auth/auth.go", `package auth

func Login(user string) error {
	return validate(user)
}

func validate(user string) error {
	return nil
}
`)
	writeWorkspaceFixture(t, callCtx, "internal/api/handler.go", `package api

import "example.com/internal/auth"

func HandleLogin(user string) error {
	if err := auth.Login(user); err != nil {
		return err
	}
	return nil
}

func Retry(user string) error {
	_ = auth.Login(user)
	_ = auth.Login(user)
	return nil
}
`)
	writeWorkspaceFixture(t, callCtx, "internal/api/handler_test.go", `package api

func TestHandleLogin(t *testing.T) {
	_ = Login("x")
}
`)
}

func TestFindCallersReportsEnclosingFunction(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_callers", `{"symbol":"Login"}`)
	if err != nil {
		t.Fatalf("find_callers: %v", err)
	}
	if !strings.Contains(out, "in HandleLogin") {
		t.Errorf("call site not attributed to its enclosing function:\n%s", out)
	}
	if !strings.Contains(out, "internal/api/handler.go:") {
		t.Errorf("calling file not reported:\n%s", out)
	}
	// The declaration itself is not a call site.
	if strings.Contains(out, "internal/auth/auth.go:3") {
		t.Errorf("declaration line reported as a call site:\n%s", out)
	}
}

func TestFindCallersFindsRepeatedCalls(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_callers", `{"symbol":"Login"}`)
	if err != nil {
		t.Fatalf("find_callers: %v", err)
	}
	if strings.Count(out, "in Retry") != 2 {
		t.Errorf("expected both calls inside Retry to be listed:\n%s", out)
	}
}

func TestFindCallersRanksTestsLast(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_callers", `{"symbol":"Login"}`)
	if err != nil {
		t.Fatalf("find_callers: %v", err)
	}
	sourceIdx := strings.Index(out, "internal/api/handler.go")
	testIdx := strings.Index(out, "internal/api/handler_test.go")
	if sourceIdx >= 0 && testIdx >= 0 && sourceIdx > testIdx {
		t.Errorf("test call sites ranked above source:\n%s", out)
	}
}

func TestFindCallersWarnsAboutNameMatching(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_callers", `{"symbol":"Login"}`)
	if err != nil {
		t.Fatalf("find_callers: %v", err)
	}
	// Resolution is lexical; the output must not imply type-accurate results.
	if !strings.Contains(out, "by name, not by type") {
		t.Errorf("missing the name-matching caveat:\n%s", out)
	}
}

func TestFindCallersNoCallSites(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_callers", `{"symbol":"NeverCalledAnywhere"}`)
	if err != nil {
		t.Fatalf("find_callers: %v", err)
	}
	if !strings.Contains(out, "No files mention") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestFindCalleesListsWhatAFunctionCalls(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_callees",
		`{"symbol":"Login","path":"internal/auth/auth.go"}`)
	if err != nil {
		t.Fatalf("find_callees: %v", err)
	}
	if !strings.Contains(out, "validate") {
		t.Errorf("callee not reported:\n%s", out)
	}
}

func TestFindCalleesCollapsesRepeatedCalls(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_callees",
		`{"symbol":"Retry","path":"internal/api/handler.go"}`)
	if err != nil {
		t.Fatalf("find_callees: %v", err)
	}
	if !strings.Contains(out, "2 calls") {
		t.Errorf("repeated calls not collapsed with a count:\n%s", out)
	}
}

// Calls in a sibling function must not be attributed to the target.
func TestFindCalleesRespectsDeclarationBoundary(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_callees",
		`{"symbol":"validate","path":"internal/auth/auth.go"}`)
	if err != nil {
		t.Fatalf("find_callees: %v", err)
	}
	if !strings.Contains(out, "makes no calls") {
		t.Errorf("validate calls nothing, but got:\n%s", out)
	}
}

// Without a path, find_callees locates the declaration itself.
func TestFindCalleesResolvesPathWhenOmitted(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_callees", `{"symbol":"HandleLogin"}`)
	if err != nil {
		t.Fatalf("find_callees: %v", err)
	}
	if !strings.Contains(out, "internal/api/handler.go") {
		t.Errorf("declaration not located without a path:\n%s", out)
	}
	if !strings.Contains(out, "Login") {
		t.Errorf("callee not reported:\n%s", out)
	}
}

// Regression: results were collected through a channel sized from the
// candidate-file count, so a single file holding more call sites than that
// buffer blocked every worker forever. A hot helper called many times in one
// file is the normal case, not an edge case.
func TestFindCallersManyCallSitesInOneFile(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)

	var body strings.Builder
	body.WriteString("package sample\n\nfunc helper() {}\n\nfunc Hot() {\n")
	for i := 0; i < 500; i++ {
		body.WriteString("\thelper()\n")
	}
	body.WriteString("}\n")
	writeWorkspaceFixture(t, callCtx, "hot.go", body.String())

	done := make(chan string, 1)
	go func() {
		out, err := execSymbolTool(t, registry, callCtx, "find_callers", `{"symbol":"helper"}`)
		if err != nil {
			done <- "ERR: " + err.Error()
			return
		}
		done <- out
	}()

	select {
	case out := <-done:
		if strings.HasPrefix(out, "ERR: ") {
			t.Fatalf("find_callers: %s", strings.TrimPrefix(out, "ERR: "))
		}
		if !strings.Contains(out, "in Hot") {
			t.Errorf("call sites not attributed:\n%s", firstLines(out, 5))
		}
	case <-time.After(60 * time.Second):
		t.Fatal("find_callers deadlocked with many call sites in one file")
	}
}

func TestFindSymbolManyDeclarationsInOneFile(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)

	var body strings.Builder
	body.WriteString("package sample\n")
	for i := 0; i < 200; i++ {
		body.WriteString("\nfunc dup() {}\n")
	}
	writeWorkspaceFixture(t, callCtx, "dupes.go", body.String())

	done := make(chan error, 1)
	go func() {
		_, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"dup"}`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("find_symbol: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("find_symbol deadlocked with many declarations in one file")
	}
}

func TestCallGraphToolsRegistered(t *testing.T) {
	registry, _ := workspaceToolTestRegistry(t)
	for _, name := range []string{"find_callers", "find_callees"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if def.Mutating {
			t.Errorf("%s must not be mutating", name)
		}
		schema, _ := def.InputSchema.(map[string]interface{})
		if schema["additionalProperties"] != false {
			t.Errorf("%s schema should forbid additional properties", name)
		}
	}
}

func TestCallGraphToolsRejectBlankSymbol(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedCallGraphWorkspace(t, callCtx)

	for _, tool := range []string{"find_callers", "find_callees"} {
		if _, err := execSymbolTool(t, registry, callCtx, tool, `{"symbol":"  "}`); err == nil {
			t.Errorf("%s should reject a blank symbol", tool)
		}
	}
}
