package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type scriptFactory struct{ model runtime.NativeModel }

func (f scriptFactory) ResolveNativeModel(context.Context, *runtime.ExecutionContext, []tools.Definition) (runtime.NativeModel, error) {
	return f.model, nil
}

type codingModel struct{ step int }

func (m *codingModel) Generate(_ context.Context, r runtime.NativeModelRequest) (*runtime.NativeModelResponse, error) {
	m.step++
	var block runtime.NativeBlock
	switch m.step {
	case 1:
		block = runtime.NativeBlock{Type: "tool_call", ToolCallID: "read", ToolName: "read_files", Input: json.RawMessage(`{"files":[{"path":"calc.py"}]}`)}
	case 2:
		block = runtime.NativeBlock{Type: "tool_call", ToolCallID: "write", ToolName: "write_file", Input: json.RawMessage(`{"path":"calc.py","content":"def add(a, b):\n    return a + b\n"}`)}
	case 3:
		block = runtime.NativeBlock{Type: "tool_call", ToolCallID: "test", ToolName: "run_command", Input: json.RawMessage(`{"program":"python3","args":["-c","from calc import add; assert add(2, 3) == 5; print('tests passed')"]}`)}
	default:
		return &runtime.NativeModelResponse{Message: runtime.NativeMessage{Role: "assistant", Content: "Fixed calc.py and validated addition."}}, nil
	}
	return &runtime.NativeModelResponse{Message: runtime.NativeMessage{Role: "assistant", Blocks: []runtime.NativeBlock{block}}}, nil
}
func TestLocalCodingPersistsAndExecutes(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "calc.py"), []byte("def add(a,b): return a-b\n"), 0600); e != nil {
		t.Fatal(e)
	}
	home := t.TempDir()
	s, e := OpenSession(home, scriptFactory{&codingModel{}})
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.Execute(context.Background(), Options{Directory: dir, Prompt: "Fix add and run tests", Yes: true})
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "completed" {
		t.Fatalf("run %s: %s", r.Status, r.ErrorMessage)
	}
	content, e := os.ReadFile(filepath.Join(dir, "calc.py"))
	if e != nil || !strings.Contains(string(content), "a + b") {
		t.Fatalf("file not fixed: %s %v", content, e)
	}
	calls, e := s.Store.ListToolCalls(context.Background(), localApp, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, c := range calls {
		if c.ToolName == "run_command" && strings.Contains(string(c.Output), "tests passed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("test command did not pass: %+v", calls)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = OpenSession(home, scriptFactory{&codingModel{}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	saved, e := s.Store.GetRun(context.Background(), localApp, r.ID)
	if e != nil || saved == nil || saved.Status != "completed" {
		t.Fatalf("run not persisted: %+v %v", saved, e)
	}
	events, e := s.Store.ListEvents(context.Background(), localApp, r.ID)
	if e != nil || len(events) == 0 {
		t.Fatalf("events not persisted: %v", e)
	}
}
func TestApprovalSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "calc.py"), []byte("def add(a,b): return a-b\n"), 0600)
	home := t.TempDir()
	model := &newFileModel{}
	s, e := OpenSession(home, scriptFactory{model})
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.Execute(context.Background(), Options{Directory: dir, Prompt: "Fix add"})
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "paused" {
		t.Fatalf("expected approval pause, got %s", r.Status)
	}
	content, _ := os.ReadFile(filepath.Join(dir, "calc.py"))
	if strings.Contains(string(content), "a + b") {
		t.Fatal("mutated before approval")
	}
	s.Close()
	s, e = OpenSession(home, scriptFactory{model})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e = s.Execute(context.Background(), Options{Resume: r.ID, Intent: "approve", Prompt: "Approved"})
	if e != nil {
		t.Fatal(e)
	}
	content, _ = os.ReadFile(filepath.Join(dir, "new.py"))
	if !strings.Contains(string(content), "a + b") {
		t.Fatalf("approved write not performed: %s; run %s", content, r.Status)
	}
}
func TestReviewCannotWrite(t *testing.T) {
	dir := t.TempDir()
	original := []byte("def add(a,b): return a-b\n")
	os.WriteFile(filepath.Join(dir, "calc.py"), original, 0600)
	s, e := OpenSession(t.TempDir(), scriptFactory{&codingModel{}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	_, e = s.Execute(context.Background(), Options{Directory: dir, Prompt: "Review", Review: true, Yes: true})
	if e != nil {
		t.Fatal(e)
	}
	content, _ := os.ReadFile(filepath.Join(dir, "calc.py"))
	if string(content) != string(original) {
		t.Fatal("review mutated workspace")
	}
}
func TestTerminalSanitization(t *testing.T) {
	if got := safeText("ok\x1b]52;c;secret\x07\r\n"); strings.ContainsAny(got, "\x1b\x07\r") {
		t.Fatalf("terminal controls survived: %q", got)
	}
}

type newFileModel struct{ called bool }

func (m *newFileModel) Generate(context.Context, runtime.NativeModelRequest) (*runtime.NativeModelResponse, error) {
	if m.called {
		return &runtime.NativeModelResponse{Message: runtime.NativeMessage{Role: "assistant", Content: "Created new.py."}}, nil
	}
	m.called = true
	return &runtime.NativeModelResponse{Message: runtime.NativeMessage{Role: "assistant", Blocks: []runtime.NativeBlock{{Type: "tool_call", ToolCallID: "new", ToolName: "write_file", Input: json.RawMessage(`{"path":"new.py","content":"def add(a,b): return a + b\n"}`)}}}}, nil
}

type blockingModel struct{ started chan struct{} }

func (m blockingModel) Generate(ctx context.Context, _ runtime.NativeModelRequest) (*runtime.NativeModelResponse, error) {
	close(m.started)
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestCancellationAndWorkspaceLock(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	started := make(chan struct{})
	s, e := OpenSession(home, scriptFactory{blockingModel{started}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan executionDone, 1)
	go func() { r, e := s.Execute(ctx, Options{Directory: dir, Prompt: "Wait"}); done <- executionDone{r, e} }()
	<-started
	other, e := OpenSession(home, scriptFactory{&codingModel{}})
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	if _, e = other.Execute(context.Background(), Options{Directory: dir, Prompt: "Conflict"}); e == nil || !strings.Contains(e.Error(), "lock workspace") {
		t.Fatalf("missing workspace lock: %v", e)
	}
	cancel()
	d := <-done
	if d.err == nil || d.run == nil || d.run.Status != "cancelled" {
		t.Fatalf("cancellation did not persist: %+v %v", d.run, d.err)
	}
	unlock, e := lockWorkspace(home, dir)
	if e != nil {
		t.Fatal("workspace not released:", e)
	}
	unlock()
}
