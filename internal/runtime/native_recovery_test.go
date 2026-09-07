package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestNativeRecoveryToolOutcomes(t *testing.T) {
	for _, test := range []struct {
		name                          string
		mutating, complete, wantError bool
	}{
		{"completed read", false, true, false}, {"completed mutation", true, true, false},
		{"interrupted read", false, false, false}, {"ambiguous mutation", true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			x := contextTestExec(t)
			count := 0
			x.Tools.Register(tools.Definition{Name: "probe", Mutating: test.mutating, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
				count++
				return json.RawMessage(`{}`), nil
			})
			r, err := openNativeRecorder(x.Context, x, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.initialMessages(true); err != nil {
				t.Fatal(err)
			}
			messages := []NativeMessage{{Role: "user", Content: "inspect"}, {Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolName: "probe", ToolCallID: "c", Input: json.RawMessage(`{}`)}}}}
			if test.complete {
				messages = append(messages, NativeMessage{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolName: "probe", ToolCallID: "c", Output: `{"ok":true}`}}})
			}
			if err := r.save(x.Context, "tools", &nativeExecutionResult{Messages: messages}); err != nil {
				t.Fatal(err)
			}
			model := &contextTestModel{}
			_, err = executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: model}})
			if (err != nil) != test.wantError {
				t.Fatalf("resume error=%v", err)
			}
			if count != 0 {
				t.Fatal("recovery reexecuted a tool")
			}
			if !test.wantError {
				if len(model.requests) != 1 {
					t.Fatal("recovery did not continue")
				}
				assertCompleteToolGroups(t, model.requests[0].Messages)
				if !test.complete && !strings.Contains(model.requests[0].Messages[2].Blocks[0].Output, "Read interrupted") {
					t.Fatal("missing read not explained")
				}
			}
		})
	}
}

func TestNativeRecoveryFinishedTurnDoesNotSampleAgain(t *testing.T) {
	x := contextTestExec(t)
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.initialMessages(true); err != nil {
		t.Fatal(err)
	}
	result := &nativeExecutionResult{TurnFinished: true, TurnOutcome: "completed", AssistantText: "done", Messages: []NativeMessage{{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolName: nativeToolFinishTurn, ToolCallID: "finish", Input: json.RawMessage(`{}`)}}}, {Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolName: nativeToolFinishTurn, ToolCallID: "finish", Output: `{"ok":true}`}}}}}
	if err := r.save(x.Context, "tools", result); err != nil {
		t.Fatal(err)
	}
	model := &contextTestModel{}
	got, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: model}})
	if err != nil || !got.TurnFinished || len(model.requests) != 0 {
		t.Fatalf("finished turn restarted: %+v %v", got, err)
	}
}

func TestNativeRecoveryUsesDurableApprovedOutcome(t *testing.T) {
	x := contextTestExec(t)
	if err := x.Store.CreateRun(x.Context, x.Run); err != nil {
		t.Fatal(err)
	}
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.initialMessages(true); err != nil {
		t.Fatal(err)
	}
	messages := []NativeMessage{{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "c", ToolName: "write", Input: json.RawMessage(`{}`)}}}, {Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: "c", ToolName: "write", Input: json.RawMessage(`{}`), Output: `{"approval_required":true,"interaction_id":"i","tool_name":"write"}`}}}, {Role: "user", Content: "approved"}}
	if err := r.save(x.Context, "approval_tools", &nativeExecutionResult{Messages: messages}); err != nil {
		t.Fatal(err)
	}
	messages[1].Blocks[0].Output = `{"document_id":"done"}`
	body, err := json.Marshal(nativeOutputSummary{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.Store.(agentcore.RunSummaryStore).UpdateRunOutputSummary(x.Context, x.AppID, x.Run.ID, body); err != nil {
		t.Fatal(err)
	}
	model := &contextTestModel{}
	if _, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: model}}); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 || !strings.Contains(model.requests[0].Messages[1].Blocks[0].Output, "done") {
		t.Fatal("durable approved result not restored")
	}
}

func TestNativeApprovalCancellation(t *testing.T) {
	for _, during := range []bool{false, true} {
		t.Run(map[bool]string{false: "before approval execution", true: "during approved mutation"}[during], func(t *testing.T) {
			x := contextTestExec(t)
			x.Run.Status = agentcore.RunStatusRunning
			count := 0
			x.Tools.Register(tools.Definition{Name: "create_document", Mutating: true, InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}, func(ctx context.Context, _ tools.CallContext, _ json.RawMessage) (json.RawMessage, error) {
				count++
				if during {
					run, err := x.Store.GetRun(ctx, x.AppID, x.Run.ID)
					if err != nil {
						return nil, err
					}
					run.Status = agentcore.RunStatusCancelled
					if err := x.Store.UpdateRun(ctx, run); err != nil {
						return nil, err
					}
				}
				return json.RawMessage(`{"document_id":"doc"}`), nil
			})
			x.Agent.AllowedTools = []string{"create_document"}
			x.AllowedTools = map[string]bool{"create_document": true}
			x.Agent.ApprovalMode = agentcore.ApprovalModeAlways
			if err := x.Store.CreateRun(x.Context, x.Run); err != nil {
				t.Fatal(err)
			}
			model := &contextApprovalModel{fakeNativeModel: fakeNativeModel{responses: []NativeModelResponse{{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "c", ToolName: "create_document", Input: json.RawMessage(`{}`)}}}}}}}
			first, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: model}})
			if err != nil || !first.AwaitingApproval {
				t.Fatalf("pause: %v", err)
			}
			interactions, err := x.Store.ListInteractions(x.Context, x.AppID, x.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			resolveInteraction(t, x.Store, x.AppID, x.Run.ID, interactions[0].ID, "approve", "")
			x.Run.Input.Metadata = map[string]any{"last_resume": map[string]any{"intent": "approve"}}
			if !during {
				run, err := x.Store.GetRun(x.Context, x.AppID, x.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				run.Status = agentcore.RunStatusCancelled
				if err := x.Store.UpdateRun(x.Context, run); err != nil {
					t.Fatal(err)
				}
			}
			continued := &contextApprovalModel{}
			_, err = executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: continued}})
			if err == nil {
				t.Fatal("cancelled run continued")
			}
			stored, err := x.Store.GetRun(x.Context, x.AppID, x.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if during {
				want = 1
			}
			if count != want || stored.Status != agentcore.RunStatusCancelled || len(continued.requests) != 0 {
				t.Fatalf("writes=%d status=%s requests=%d", count, stored.Status, len(continued.requests))
			}
		})
	}
}
