package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// TestBatchStopsEarlyBaseline records current behavior for the work-job plan
// (phase 0): a run asked to handle ten items records three, then claims it is
// done. Neither implicit nor explicit completion knows about items, so the
// run completes and seven items are silently missing. Work-job children must
// not inherit this: their completion is decided by accepted item results, not
// by finish_turn. When that rule lands, job-child tests assert the opposite;
// ordinary runs keep this behavior.
func TestBatchStopsEarlyBaseline(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			var recorded []string
			registry := tools.NewRegistry()
			registry.Register(tools.Definition{Name: "record_item", Description: "Record one item result", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"item": map[string]any{"type": "string"}}}}, func(_ context.Context, _ tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
				var body struct {
					Item string `json:"item"`
				}
				_ = json.Unmarshal(input, &body)
				recorded = append(recorded, body.Item)
				return json.RawMessage(`{"ok":true}`), nil
			})
			var calls []NativeBlock
			for i := 1; i <= 3; i++ {
				calls = append(calls, NativeBlock{Type: nativeBlockTypeToolCall, ToolCallID: fmt.Sprintf("c%d", i), ToolName: "record_item", Input: json.RawMessage(fmt.Sprintf(`{"item":"school-%d"}`, i))})
			}
			final := NativeMessage{Role: "assistant", Content: "Processed all schools."}
			if explicit {
				final.Blocks = []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "finish", ToolName: nativeToolFinishTurn, Input: json.RawMessage(`{"outcome":"completed","summary":"Processed all schools."}`)}}
			}
			model := &fakeNativeModel{responses: []NativeModelResponse{
				{Message: NativeMessage{Role: "assistant", Blocks: calls}},
				{Message: final},
			}}
			run := &agentcore.AgentRun{ID: "batch", AppID: "app-a", RuntimeKind: agentcore.RuntimeNativeSDK, Target: agentcore.TargetRef{Type: "batch", ID: "b-1"},
				Input: agentcore.RunInput{Instructions: "Research school-1 through school-10 and call record_item once per school."}}
			if explicit {
				run.Input.TurnPolicy = agentcore.TurnPolicy{CompletionMode: agentcore.TurnCompletionExplicit, MaxCompletionCorrections: 2}
			}
			adapter := NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: model}, MaxToolSteps: 8})
			result, err := adapter.Execute(&ExecutionContext{
				Context: context.Background(), AppID: "app-a", Store: store.NewMemory(), Run: run,
				Agent: &agentcore.Agent{Name: "Batch", RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: []string{"record_item"}},
				Tools: registry, AllowedTools: map[string]bool{"record_item": true},
			})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if len(recorded) != 3 || len(model.requests) != 2 {
				t.Fatalf("recorded %v over %d requests", recorded, len(model.requests))
			}
			if explicit && (!result.TurnFinished || result.TurnOutcome != "completed") {
				t.Fatalf("explicit run should accept the early finish today: %#v", result)
			}
		})
	}
}
