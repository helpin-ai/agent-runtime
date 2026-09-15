package runtime

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestNativeCheckpointWithoutCompactionRestoresCompletedTurn(t *testing.T) {
	x := contextTestExec(t)
	x.Agent.ExecutionConfig = json.RawMessage(`{}`)
	m := &contextTestModel{}
	first, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}})
	if err != nil {
		t.Fatal(err)
	}
	requests := len(m.requests)
	second, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != requests || second.AssistantText != first.AssistantText || len(second.Messages) == 0 {
		t.Fatal("completed checkpoint was resampled or lost its transcript")
	}
	x.Run.Input.Metadata = map[string]any{"last_resume": map[string]any{"intent": "reply", "content": "Continue with another task"}}
	if _, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}}); err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != requests+1 || len(m.requests[requests].Messages) <= len(first.Messages) {
		t.Fatal("resume did not carry forward the saved transcript")
	}
}

func TestNativeCheckpointWithoutCompactionRejectsStaleWriter(t *testing.T) {
	x := contextTestExec(t)
	a, err := openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	result := &nativeExecutionResult{Messages: []NativeMessage{{Role: "user", Content: "keep"}}}
	if err := a.save(x.Context, "done", result); err != nil {
		t.Fatal(err)
	}
	if err := b.save(x.Context, "done", result); !errors.Is(err, agentcore.ErrNativeStateConflict) {
		t.Fatalf("stale checkpoint error = %v", err)
	}
}

func TestNativeCheckpointLoadsLegacyUnmanagedHistory(t *testing.T) {
	x := contextTestExec(t)
	x.Run.OutputSummary = json.RawMessage(`{"native_messages":[{"role":"user","content":"old request"},{"role":"assistant","content":"old result"}]}`)
	x.Run.Input.Metadata = map[string]any{"last_resume": map[string]any{"intent": "reply", "content": "continue"}}
	payload := json.RawMessage(`{"format":1,"managed":false,"phase":"done"}`)
	record := agentcore.NativeState{AppID: x.AppID, RunID: x.Run.ID, Payload: payload}
	if err := x.Store.(agentcore.NativeStateStore).SaveNativeState(x.Context, &record, nil); err != nil {
		t.Fatal(err)
	}
	r, err := openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	messages, _, err := r.initialMessages(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || messages[0].Content != "old request" {
		t.Fatalf("legacy history lost: %+v", messages)
	}
}
