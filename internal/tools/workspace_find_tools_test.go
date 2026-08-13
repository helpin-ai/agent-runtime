package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/symbols"
)

func requireRipgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not in PATH")
	}
}

func seedFindSymbolWorkspace(t *testing.T, callCtx CallContext) {
	t.Helper()
	writeWorkspaceFixture(t, callCtx, "internal/config/config.go", `package config

// Config holds settings.
type Config struct {
	Name string
}

func NewConfig(name string) *Config {
	return &Config{Name: name}
}
`)
	writeWorkspaceFixture(t, callCtx, "internal/server/server.go", `package server

import "example.com/internal/config"

func Serve(cfg *config.Config) error {
	_ = config.NewConfig("x")
	return nil
}
`)
	writeWorkspaceFixture(t, callCtx, "internal/config/config_test.go", `package config

func NewConfig(name string) *Config { return nil }
`)
	writeWorkspaceFixture(t, callCtx, "web/panel.tsx", `import { useState } from "react";

export function Panel() {
  const [open, setOpen] = useState(false);
  return open;
}
`)
}

func TestFindSymbolLocatesDeclarationAcrossFiles(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedFindSymbolWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"NewConfig"}`)
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	if !strings.Contains(out, "internal/config/config.go:8") {
		t.Errorf("declaration not located:\n%s", out)
	}
	// The file that only calls it must not be reported.
	if strings.Contains(out, "internal/server/server.go") {
		t.Errorf("a calling file was reported as a declaration:\n%s", out)
	}
}

// The ranking contract: real source outranks tests for the same name.
func TestFindSymbolRanksTestsBelowSource(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedFindSymbolWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"NewConfig"}`)
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	sourceIdx := strings.Index(out, "internal/config/config.go")
	testIdx := strings.Index(out, "internal/config/config_test.go")
	if sourceIdx < 0 || testIdx < 0 {
		t.Fatalf("expected both declarations to be reported:\n%s", out)
	}
	if sourceIdx > testIdx {
		t.Errorf("test declaration ranked above source:\n%s", out)
	}
}

// The case that motivated skipping a whole-repo index: a name used everywhere
// and declared nowhere must resolve quickly and say so plainly.
func TestFindSymbolImportedButNeverDeclared(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedFindSymbolWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"useState"}`)
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	if !strings.Contains(out, "declared in none") {
		t.Errorf("expected an explicit 'declared nowhere' answer:\n%s", out)
	}
}

func TestFindSymbolUnknownName(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedFindSymbolWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"NoSuchThing"}`)
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	if !strings.Contains(out, "No files mention") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestFindSymbolKindFilter(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedFindSymbolWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"Config","kind":"type"}`)
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	if !strings.Contains(out, "[type, lines") {
		t.Errorf("expected the type declaration:\n%s", out)
	}

	out, err = execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"Config","kind":"interface"}`)
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	if !strings.Contains(out, "No declaration") {
		t.Errorf("kind filter did not exclude the type:\n%s", out)
	}
}

// find_symbol reports paths relative to the workspace root so its output can be
// fed straight into read_symbol without translation.
func TestFindSymbolOutputFeedsReadSymbol(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedFindSymbolWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"NewConfig"}`)
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	var reported string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "internal/config/config.go:") {
			reported = strings.SplitN(trimmed, ":", 2)[0]
			break
		}
	}
	if reported == "" {
		t.Fatalf("no path reported:\n%s", out)
	}
	input, err := json.Marshal(map[string]string{"path": reported, "symbol": "NewConfig"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_symbol", input); err != nil {
		t.Fatalf("read_symbol could not consume find_symbol's path %q: %v", reported, err)
	}
}

func TestFindSymbolRejectsBlankAndUnknownFields(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedFindSymbolWorkspace(t, callCtx)

	if _, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"   "}`); err == nil {
		t.Error("expected a blank name to be rejected")
	}
	if _, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"Config","path":"x"}`); err == nil {
		t.Error("expected strict decoding to reject an unknown field")
	}
}

// A symbol name is model-supplied input; regex metacharacters in it must not
// reach ripgrep as pattern syntax.
func TestFindSymbolEscapesRegexMetacharacters(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedFindSymbolWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":".*"}`)
	if err != nil {
		t.Fatalf("find_symbol should not error on metacharacters: %v", err)
	}
	if strings.Contains(out, "config.go:") {
		t.Errorf("metacharacters were treated as a pattern:\n%s", out)
	}
}

func TestFindSymbolRegistered(t *testing.T) {
	registry, _ := workspaceToolTestRegistry(t)
	def, ok := registry.Definition("find_symbol")
	if !ok {
		t.Fatal("find_symbol not registered")
	}
	if def.Mutating {
		t.Error("find_symbol must not be mutating")
	}
	schema, _ := def.InputSchema.(map[string]interface{})
	if schema["additionalProperties"] != false {
		t.Error("find_symbol schema should forbid additional properties")
	}
}

// A candidate ripgrep selects but the parser cannot handle must be reported.
// Silently dropping it turns "we could not look" into "it is not there".
func TestFindSymbolReportsSkippedCandidates(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)

	// Larger than symbols.MaxParseBytes, but ripgrep still matches the
	// declaration inside it, so it arrives as a candidate.
	var oversized strings.Builder
	oversized.WriteString("package sample\n\nfunc Gigantic() {}\n")
	filler := strings.Repeat("// filler filler filler filler filler filler\n", 1)
	for oversized.Len() <= symbols.MaxParseBytes {
		oversized.WriteString(filler)
	}
	writeWorkspaceFixture(t, callCtx, "generated.go", oversized.String())

	out, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"Gigantic"}`)
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	if !strings.Contains(out, "skipped") {
		t.Errorf("skipped candidate not reported:\n%s", out)
	}
	if !strings.Contains(out, "too large to parse") {
		t.Errorf("skip reason not reported:\n%s", out)
	}
	// The negative result must not read as a confident "does not exist".
	if !strings.Contains(out, "would not appear above") {
		t.Errorf("skip note should qualify the result:\n%s", out)
	}
}

func TestFindCallersReportsSkippedCandidates(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)

	var oversized strings.Builder
	oversized.WriteString("package sample\n\nfunc caller() { Gigantic() }\n")
	filler := strings.Repeat("// filler filler filler filler filler filler\n", 1)
	for oversized.Len() <= symbols.MaxParseBytes {
		oversized.WriteString(filler)
	}
	writeWorkspaceFixture(t, callCtx, "generated.go", oversized.String())

	out, err := execSymbolTool(t, registry, callCtx, "find_callers", `{"symbol":"Gigantic"}`)
	if err != nil {
		t.Fatalf("find_callers: %v", err)
	}
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "too large to parse") {
		t.Errorf("skipped candidate not reported:\n%s", out)
	}
}

// A clean run must not carry a skip note.
func TestFindSymbolOmitsSkipNoteWhenNothingSkipped(t *testing.T) {
	requireRipgrep(t)
	registry, callCtx := workspaceToolTestRegistry(t)
	seedFindSymbolWorkspace(t, callCtx)

	out, err := execSymbolTool(t, registry, callCtx, "find_symbol", `{"name":"NewConfig"}`)
	if err != nil {
		t.Fatalf("find_symbol: %v", err)
	}
	if strings.Contains(out, "skipped") {
		t.Errorf("unexpected skip note on a clean run:\n%s", out)
	}
}

// ripgrep writes the real reason to stderr; surfacing only "exit status 2"
// leaves nothing to act on.
func TestRipgrepFileListSurfacesStderr(t *testing.T) {
	requireRipgrep(t)
	// An unbalanced group is invalid regex, so rg exits 2 with a parse error.
	_, err := ripgrepFileList(context.Background(), "(", false, t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected an error for an invalid pattern")
	}
	if !strings.Contains(err.Error(), "regex parse error") {
		t.Errorf("ripgrep's diagnostic was not surfaced, got: %v", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error should be collapsed to one line, got: %q", err.Error())
	}
}

func TestRipgrepFileListNoMatchIsNotAnError(t *testing.T) {
	requireRipgrep(t)
	files, err := ripgrepFileList(context.Background(), "NothingMatchesThis", true, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("no-match should not be an error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected no files, got %v", files)
	}
}
