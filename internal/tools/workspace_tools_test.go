package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestWorkspaceToolWriteFileRequiresPriorReadForExistingFile(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "existing.txt"), []byte("original"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"existing.txt","content":"updated"}`))
	if err == nil || !strings.Contains(err.Error(), "must read existing.txt before modifying it") {
		t.Fatalf("expected prior-read error, got %v", err)
	}
}

func TestWorkspaceToolWriteFileRejectsStaleExistingFile(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	filePath := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "existing.txt")
	if err := os.WriteFile(filePath, []byte("original"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"existing.txt"}`)); err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}
	staleModTime := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(filePath, []byte("changed elsewhere"), 0644); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}
	if err := os.Chtimes(filePath, staleModTime, staleModTime); err != nil {
		t.Fatalf("set stale mod time: %v", err)
	}

	_, err := registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"existing.txt","content":"updated"}`))
	if err == nil || !strings.Contains(err.Error(), "changed since the last read_file call") {
		t.Fatalf("expected stale-read error, got %v", err)
	}
}

func TestWorkspaceToolWriteFileAllowsCreateWithoutPriorRead(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	output, err := registry.Execute(context.Background(), callCtx, "write_file", json.RawMessage(`{"path":"new.txt","content":"created"}`))
	if err != nil {
		t.Fatalf("write_file returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, output), "Wrote 7 bytes to new.txt") {
		t.Fatalf("unexpected output: %s", string(output))
	}
	data, err := os.ReadFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "new.txt"))
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(data) != "created" {
		t.Fatalf("unexpected created file content %q", string(data))
	}
}

func TestWorkspaceToolEditFileRequiresUniqueMatch(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	filePath := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt")
	if err := os.WriteFile(filePath, []byte("alpha\nshared\nomega\nshared\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"sample.txt"}`)); err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}

	_, err := registry.Execute(context.Background(), callCtx, "edit_file", json.RawMessage(`{"path":"sample.txt","old_string":"shared","new_string":"updated"}`))
	if err == nil || !strings.Contains(err.Error(), "matched 2 locations") {
		t.Fatalf("expected non-unique match error, got %v", err)
	}
}

func TestWorkspaceToolEditFileAppliesSingleReplacementAfterRead(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	filePath := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt")
	initial := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(filePath, []byte(initial), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"sample.txt"}`)); err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}

	output, err := registry.Execute(context.Background(), callCtx, "edit_file", json.RawMessage(`{"path":"sample.txt","old_string":"alpha\nbeta\ngamma\n","new_string":"alpha\nbeta-updated\ngamma\n"}`))
	if err != nil {
		t.Fatalf("edit_file returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, output), "Edited sample.txt by replacing 1 occurrence.") {
		t.Fatalf("unexpected output: %s", string(output))
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	if string(data) != "alpha\nbeta-updated\ngamma\n" {
		t.Fatalf("unexpected edited content %q", string(data))
	}
}

func TestWorkspaceToolReadFileDefaultsToBoundedWindow(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	filePath := filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "large.txt")
	var builder strings.Builder
	for i := 1; i <= defaultReadFileLimitLines+10; i++ {
		builder.WriteString("line ")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteString("\n")
	}
	if err := os.WriteFile(filePath, []byte(builder.String()), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	output, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"large.txt"}`))
	if err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}
	text := workspaceToolString(t, output)
	if !strings.Contains(text, `<file path="large.txt" start_line="1" returned_lines="120">`) {
		t.Fatalf("expected bounded read metadata, got %q", text)
	}
	if !strings.Contains(text, "line 1") || !strings.Contains(text, "line 120") {
		t.Fatalf("expected first window content, got %q", text)
	}
	if strings.Contains(text, "line 125") {
		t.Fatalf("did not expect lines past default window, got %q", text)
	}
	if !strings.Contains(text, `"offset_line":121`) {
		t.Fatalf("expected continuation hint, got %q", text)
	}
}

func TestWorkspaceToolReadFilesReadsMultipleFiles(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("a\nb\nc\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.txt"), []byte("x\ny\nz\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	output, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"one.txt","limit_lines":2},{"path":"two.txt","offset_line":2,"limit_lines":2}]}`))
	if err != nil {
		t.Fatalf("read_files returned error: %v", err)
	}
	text := workspaceToolString(t, output)
	if !strings.Contains(text, `<files count="2">`) || !strings.Contains(text, "a\nb") || !strings.Contains(text, "y\nz") {
		t.Fatalf("unexpected output: %q", text)
	}
}

func TestWorkspaceToolReadFileRejectsPathTraversal(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	_, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"../outside.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "path traversal not allowed") {
		t.Fatalf("expected traversal error, got %v", err)
	}
}

func TestWorkspaceToolReadFileRejectsOversizedLimit(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt"), []byte("one\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	limit := strconv.Itoa(maxReadFileLimitLines + 1)
	_, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"sample.txt","limit_lines":`+limit+`}`))
	if err == nil || !strings.Contains(err.Error(), "limit_lines too large") {
		t.Fatalf("expected limit error, got %v", err)
	}
}

func TestWorkspaceToolListDirectoryReturnsContents(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	if err := os.Mkdir(filepath.Join(root, "nested"), 0755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	output, err := registry.Execute(context.Background(), callCtx, "list_directory", json.RawMessage(`{"path":""}`))
	if err != nil {
		t.Fatalf("list_directory returned error: %v", err)
	}
	text := workspaceToolString(t, output)
	if !strings.Contains(text, "nested/") || !strings.Contains(text, "sample.txt") {
		t.Fatalf("expected directory contents, got %q", text)
	}
}

func TestWorkspaceToolGrepAndSymbols(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	source := "package sample\n\ntype Thing struct{}\n\nfunc Run() {}\n"
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.go"), []byte(source), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	grepOut, err := registry.Execute(context.Background(), callCtx, "grep", json.RawMessage(`{"pattern":"Thing","include":"*.go"}`))
	if err != nil {
		t.Fatalf("grep returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, grepOut), "sample.go:3: type Thing struct{}") {
		t.Fatalf("unexpected grep output: %s", string(grepOut))
	}
	symbolsOut, err := registry.Execute(context.Background(), callCtx, "list_symbols", json.RawMessage(`{"path":"sample.go"}`))
	if err != nil {
		t.Fatalf("list_symbols returned error: %v", err)
	}
	symbols := workspaceToolString(t, symbolsOut)
	if !strings.Contains(symbols, "type Thing struct{}") || !strings.Contains(symbols, "func Run() {}") {
		t.Fatalf("unexpected symbols output: %q", symbols)
	}
}

func TestWorkspaceToolDefinitionsAreRegisteredWithMutatingFlags(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"read_file", "read_files", "list_directory", "search_files", "read_file_range", "ripgrep", "grep", "list_symbols"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s definition", name)
		}
		if def.Mutating {
			t.Fatalf("expected %s to be read-only", name)
		}
	}
	for _, name := range []string{"write_file", "edit_file"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s definition", name)
		}
		if !def.Mutating {
			t.Fatalf("expected %s to be mutating", name)
		}
	}
}

func workspaceToolTestRegistry(t *testing.T) (*Registry, CallContext) {
	t.Helper()
	root := t.TempDir()
	registry := NewRegistry()
	run := &agentcore.AgentRun{
		ID:    "run-1",
		AppID: "app-a",
		WorkspaceLease: &agentcore.WorkspaceLease{
			ID:       "lease-1",
			RootPath: root,
		},
	}
	return registry, CallContext{AppID: run.AppID, RunID: run.ID, Run: run}
}

func workspaceToolString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode tool output: %v; raw=%s", err, string(raw))
	}
	return out
}
