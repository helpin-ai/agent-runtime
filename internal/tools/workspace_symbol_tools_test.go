package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const symbolFixtureGo = `package sample

import "fmt"

// Config holds runner settings.
// It spans two comment lines on purpose.
type Config struct {
	Name string
}

// New builds a Config.
func New(name string) *Config {
	local := name
	fmt.Println(local)
	return &Config{Name: name}
}

func (c *Config) Describe() string {
	return c.Name
}
`

func writeWorkspaceFixture(t *testing.T, callCtx CallContext, name, content string) string {
	t.Helper()
	path := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func execSymbolTool(t *testing.T, registry *Registry, callCtx CallContext, tool, input string) (string, error) {
	t.Helper()
	raw, err := registry.Execute(context.Background(), callCtx, tool, json.RawMessage(input))
	if err != nil {
		return "", err
	}
	return workspaceToolString(t, raw), nil
}

func TestReadSymbolReturnsWholeDeclaration(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "sample.go", symbolFixtureGo)

	out, err := execSymbolTool(t, registry, callCtx, "read_symbol", `{"path":"sample.go","symbol":"New"}`)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "function New in") {
		t.Errorf("missing symbol header:\n%s", out)
	}
	// The declaration runs 12..16 with the doc comment at 11.
	for _, want := range []string{"// New builds a Config.", "func New(name string) *Config {", "return &Config{Name: name}"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The next declaration must not bleed in.
	if strings.Contains(out, "func (c *Config) Describe()") {
		t.Errorf("read past the end of the symbol:\n%s", out)
	}
}

func TestReadSymbolIncludesDocstringByDefaultAndCanSkipIt(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "sample.go", symbolFixtureGo)

	withDoc, err := execSymbolTool(t, registry, callCtx, "read_symbol", `{"path":"sample.go","symbol":"Config"}`)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(withDoc, "// Config holds runner settings.") {
		t.Errorf("doc comment not included by default:\n%s", withDoc)
	}
	if !strings.Contains(withDoc, "// It spans two comment lines on purpose.") {
		t.Errorf("multi-line doc comment truncated:\n%s", withDoc)
	}

	withoutDoc, err := execSymbolTool(t, registry, callCtx, "read_symbol",
		`{"path":"sample.go","symbol":"Config","include_docstring":false}`)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if strings.Contains(withoutDoc, "// Config holds runner settings.") {
		t.Errorf("doc comment leaked when disabled:\n%s", withoutDoc)
	}
	if !strings.Contains(withoutDoc, "type Config struct {") {
		t.Errorf("declaration missing:\n%s", withoutDoc)
	}
}

func TestReadSymbolMethodByName(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "sample.go", symbolFixtureGo)

	out, err := execSymbolTool(t, registry, callCtx, "read_symbol", `{"path":"sample.go","symbol":"Describe"}`)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "method Describe in") {
		t.Errorf("expected method header:\n%s", out)
	}
	if !strings.Contains(out, "return c.Name") {
		t.Errorf("method body missing:\n%s", out)
	}
}

// A declaration longer than the per-call cap must behave exactly like a
// truncated read_file: return the head and say where to resume.
func TestReadSymbolLongerThanLineCapAdvertisesContinuation(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	var body strings.Builder
	body.WriteString("package sample\n\nfunc Huge() {\n")
	for i := 0; i < maxReadFileLimitLines+80; i++ {
		body.WriteString(fmt.Sprintf("\tprintln(%d)\n", i))
	}
	body.WriteString("}\n")
	writeWorkspaceFixture(t, callCtx, "huge.go", body.String())

	out, err := execSymbolTool(t, registry, callCtx, "read_symbol", `{"path":"huge.go","symbol":"Huge"}`)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "offset_line=") {
		t.Errorf("expected a continuation hint:\n%s", out)
	}
	if !strings.Contains(out, "func Huge() {") {
		t.Errorf("expected the head of the declaration:\n%s", out)
	}
}

func TestReadSymbolReportsDuplicateNames(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "dup.go", "package p\n\nfunc Dup() {}\n\ntype Dup struct{}\n")

	out, err := execSymbolTool(t, registry, callCtx, "read_symbol", `{"path":"dup.go","symbol":"Dup"}`)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "other declarations share this name") {
		t.Errorf("ambiguity not reported:\n%s", out)
	}
}

func TestReadSymbolUnknownNameSuggestsAlternatives(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "sample.go", symbolFixtureGo)

	_, err := execSymbolTool(t, registry, callCtx, "read_symbol", `{"path":"sample.go","symbol":"Describ"}`)
	if err == nil {
		t.Fatal("expected an error for an unknown symbol")
	}
	if !strings.Contains(err.Error(), "Describe") {
		t.Errorf("error should suggest the near match, got: %v", err)
	}
}

func TestReadSymbolUnsupportedExtensionIsActionable(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "notes.md", "# Title\n")

	_, err := execSymbolTool(t, registry, callCtx, "read_symbol", `{"path":"notes.md","symbol":"Title"}`)
	if err == nil {
		t.Fatal("expected an error for an unsupported extension")
	}
	if !strings.Contains(err.Error(), "list_symbols") {
		t.Errorf("error should point at a working alternative, got: %v", err)
	}
}

// read_symbol must satisfy the write tools' prior-read requirement for the
// range it actually returned, exactly as read_file does.
func TestReadSymbolRecordsTheReadForLaterEdits(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "sample.go", symbolFixtureGo)

	if _, err := execSymbolTool(t, registry, callCtx, "read_symbol", `{"path":"sample.go","symbol":"Describe"}`); err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	_, err := registry.Execute(context.Background(), callCtx, "edit_file",
		json.RawMessage(`{"path":"sample.go","old_string":"return c.Name","new_string":"return c.Name + \"!\""}`))
	if err != nil {
		t.Fatalf("edit after read_symbol should be allowed, got: %v", err)
	}
}

func TestReadSymbolRejectsPathEscape(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	outside := filepath.Join(filepath.Dir(callCtx.Run.WorkspaceLease.RootPath), "outside.go")
	if err := os.WriteFile(outside, []byte("package p\nfunc Secret() {}\n"), 0o644); err != nil {
		t.Fatalf("write outside fixture: %v", err)
	}
	for _, path := range []string{"../outside.go", "/etc/passwd"} {
		if _, err := execSymbolTool(t, registry, callCtx, "read_symbol",
			fmt.Sprintf(`{"path":%q,"symbol":"Secret"}`, path)); err == nil {
			t.Errorf("expected %s to be rejected", path)
		}
	}
}

func TestReadSymbolRejectsUnknownFields(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "sample.go", symbolFixtureGo)

	_, err := execSymbolTool(t, registry, callCtx, "read_symbol",
		`{"path":"sample.go","symbol":"New","offset_line":2}`)
	if err == nil {
		t.Fatal("expected strict decoding to reject an unknown field")
	}
}

func TestReadSymbolRequiresSymbol(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "sample.go", symbolFixtureGo)

	if _, err := execSymbolTool(t, registry, callCtx, "read_symbol", `{"path":"sample.go","symbol":"  "}`); err == nil {
		t.Fatal("expected a blank symbol to be rejected")
	}
}

func TestListSymbolsReportsKindsAndRanges(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "sample.go", symbolFixtureGo)

	out, err := execSymbolTool(t, registry, callCtx, "list_symbols", `{"path":"sample.go"}`)
	if err != nil {
		t.Fatalf("list_symbols: %v", err)
	}
	// The legacy "%4d | <source line>" shape is preserved.
	if !strings.Contains(out, "| type Config struct {") {
		t.Errorf("legacy outline shape lost:\n%s", out)
	}
	if !strings.Contains(out, "[type Config, lines") {
		t.Errorf("kind and range annotation missing:\n%s", out)
	}
	if !strings.Contains(out, "[method Describe, lines") {
		t.Errorf("method not reported:\n%s", out)
	}
	// Function-local bindings must not appear.
	if strings.Contains(out, "local :=") {
		t.Errorf("function-local binding leaked into the outline:\n%s", out)
	}
}

// A file type with no grammar linked in must keep working via the legacy scan
// rather than losing the capability.
func TestListSymbolsFallsBackForUngrammaredExtension(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "notes.md", "# Title\n")

	_, err := execSymbolTool(t, registry, callCtx, "list_symbols", `{"path":"notes.md"}`)
	if err == nil {
		t.Fatal("expected unsupported-file-type error for .md")
	}
	if !strings.Contains(err.Error(), "unsupported file type") {
		t.Errorf("unexpected error text: %v", err)
	}
}

func TestListSymbolsEmptyFile(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "empty.go", "package p\n")

	out, err := execSymbolTool(t, registry, callCtx, "list_symbols", `{"path":"empty.go"}`)
	if err != nil {
		t.Fatalf("list_symbols: %v", err)
	}
	if !strings.Contains(out, "No symbols found.") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestListSymbolsRejectsBinaryFile(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	writeWorkspaceFixture(t, callCtx, "blob.go", "package p\x00\x00binary")

	if _, err := execSymbolTool(t, registry, callCtx, "list_symbols", `{"path":"blob.go"}`); err == nil {
		t.Fatal("expected binary content to be rejected")
	}
}

func TestSymbolToolsResolveRepositoryAlias(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	// An unknown alias must fail rather than silently reading the default root.
	writeWorkspaceFixture(t, callCtx, "sample.go", symbolFixtureGo)

	if _, err := execSymbolTool(t, registry, callCtx, "read_symbol",
		`{"path":"sample.go","symbol":"New","repo_alias":"nope"}`); err == nil {
		t.Fatal("expected an unknown repo alias to be rejected")
	}
}

func TestSymbolToolsRegistered(t *testing.T) {
	registry, _ := workspaceToolTestRegistry(t)
	for _, name := range []string{"read_symbol", "list_symbols"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s to be registered", name)
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
