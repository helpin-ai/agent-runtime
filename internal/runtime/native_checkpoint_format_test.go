package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// A checkpoint written before started-call markers existed cannot prove that
// an interrupted mutating call never launched, so recovery must report an
// unknown outcome rather than "not started".
func TestNativeRecoveryLegacyCheckpointReportsUnknownOutcome(t *testing.T) {
	x := contextTestExec(t)
	x.Agent.ExecutionConfig = json.RawMessage(`{}`)
	count := 0
	x.Tools.Register(tools.Definition{Name: "run_command", Mutating: true, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		count++
		return json.RawMessage(`{}`), nil
	})
	r, err := openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.initialMessages(false); err != nil {
		t.Fatal(err)
	}
	messages := []NativeMessage{{Role: "user", Content: "push it"}, {Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolName: "run_command", ToolCallID: "c", Input: json.RawMessage(`{}`)}}}}
	if err := r.save(x.Context, "tools", &nativeExecutionResult{Messages: messages}); err != nil {
		t.Fatal(err)
	}
	// Rewrite the stored checkpoint as the pre-marker format.
	store := x.Store.(agentcore.NativeStateStore)
	stored, err := store.LoadNativeState(x.Context, x.AppID, x.Run.ID)
	if err != nil || stored == nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(stored.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["format"] = json.RawMessage(`1`)
	delete(payload, "started_calls")
	if stored.Payload, err = json.Marshal(payload); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveNativeState(x.Context, stored, nil); err != nil {
		t.Fatal(err)
	}
	model := &contextTestModel{}
	if _, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: model}}); err != nil {
		t.Fatalf("resume error=%v", err)
	}
	if count != 0 {
		t.Fatal("recovery reexecuted the command")
	}
	if len(model.requests) != 1 {
		t.Fatal("recovery did not continue")
	}
	output := model.requests[0].Messages[2].Blocks[0].Output
	if !strings.Contains(output, "outcome unknown") || strings.Contains(output, "was not started") {
		t.Fatalf("legacy checkpoint must report unknown outcome, got %q", output)
	}
}

func TestNativeWorkspaceContextAnalysisLease(t *testing.T) {
	x := &ExecutionContext{WorkspaceLease: &agentcore.WorkspaceLease{ID: "analysis-1", Provider: "analysis", RootPath: t.TempDir()}}
	got := nativeWorkspaceContext(x)
	if !strings.Contains(got, "Analysis workspace") || strings.Contains(got, "Do not call repository discovery") {
		t.Fatalf("analysis lease must not claim a checkout, got %q", got)
	}
}
