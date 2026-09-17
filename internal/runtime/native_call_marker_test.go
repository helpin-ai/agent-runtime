package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestInterruptedExternalCallsResumeWithoutReplay(t *testing.T) {
	for _, name := range []string{"run_command", "run_python", "commit_and_push", "open_pr"} {
		t.Run(name, func(t *testing.T) {
			x := contextTestExec(t)
			x.Agent.ExecutionConfig = json.RawMessage(`{}`)
			count := 0
			x.Tools.Register(tools.Definition{Name: name, Mutating: true}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
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
			call := NativeBlock{Type: nativeBlockTypeToolCall, ToolName: name, ToolCallID: "external", Input: json.RawMessage(`{}`)}
			result := &nativeExecutionResult{Messages: []NativeMessage{{Role: "user", Content: "Do it"}, {Role: "assistant", Blocks: []NativeBlock{call}}}}
			if err := r.save(x.Context, "tools", result); err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(x.Context, nativeCallRecorderKey{}, &nativeCallRecorder{recorder: r, result: result})
			if err := nativeMarkCallStarted(ctx, call); err != nil {
				t.Fatal(err)
			}
			unknown, err := NativeInterruptedEffects(ctx, x.Store, x.AppID, x.Run.ID)
			if err != nil || !strings.Contains(string(unknown), "started_outcome_unknown") {
				t.Fatalf("cancel summary %s %v", unknown, err)
			}
			// A fresh model execution sees the checkpoint left by a lost worker.
			model := &contextTestModel{}
			if _, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: model}}); err != nil {
				t.Fatal(err)
			}
			if count != 0 || len(model.requests) != 1 {
				t.Fatalf("replayed=%d requests=%d", count, len(model.requests))
			}
			assertCompleteToolGroups(t, model.requests[0].Messages)
			if !strings.Contains(model.requests[0].Messages[2].Blocks[0].Output, "outcome unknown") {
				t.Fatal("uncertainty not delivered to model")
			}
		})
	}
}

func TestExternalMarkerIsDurableBeforeLaunch(t *testing.T) {
	x := contextTestExec(t)
	x.Agent.ApprovalMode = agentcore.ApprovalModeNever
	x.AllowedTools = map[string]bool{"run_command": true}
	r, err := openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	result := &nativeExecutionResult{Messages: []NativeMessage{{Role: "user", Content: "run command"}}}
	if err := r.save(x.Context, "tools", result); err != nil {
		t.Fatal(err)
	}
	x.Tools.Register(tools.Definition{Name: "run_command", Mutating: true}, func(ctx context.Context, _ tools.CallContext, _ json.RawMessage) (json.RawMessage, error) {
		fresh, err := openNativeRecorder(ctx, x, false)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.state.StartedCalls["call"].ToolName != "run_command" {
			t.Fatal("launch preceded durable marker")
		}
		return json.RawMessage(`{}`), nil
	})
	ctx := context.WithValue(x.Context, nativeCallRecorderKey{}, &nativeCallRecorder{recorder: r, result: result})
	got := executeSingleNativeToolCall(ctx, x, NativeBlock{ToolCallID: "call", ToolName: "run_command", Input: json.RawMessage(`{}`)})
	if got.IsError {
		t.Fatal(got.Output)
	}
}

type markerHookStore struct {
	*store.Memory
	beforeSave func(*agentcore.NativeState) error
	afterSave  func()
}

func (s *markerHookStore) SaveNativeState(ctx context.Context, state *agentcore.NativeState, entries []json.RawMessage) error {
	if s.beforeSave != nil {
		if err := s.beforeSave(state); err != nil {
			return err
		}
	}
	if err := s.Memory.SaveNativeState(ctx, state, entries); err != nil {
		return err
	}
	if s.afterSave != nil {
		s.afterSave()
	}
	return nil
}

func TestExternalMarkerFailureAndConcurrentCancelPreventLaunch(t *testing.T) {
	for _, scenario := range []string{"checkpoint failure", "cancel during checkpoint"} {
		t.Run(scenario, func(t *testing.T) {
			x := contextTestExec(t)
			x.Agent.ApprovalMode = agentcore.ApprovalModeNever
			x.AllowedTools = map[string]bool{"run_command": true}
			memory := &markerHookStore{Memory: store.NewMemory()}
			x.Store = memory
			x.Run.Status = agentcore.RunStatusRunning
			if err := memory.CreateRun(x.Context, x.Run); err != nil {
				t.Fatal(err)
			}
			r, err := openNativeRecorder(x.Context, x, false)
			if err != nil {
				t.Fatal(err)
			}
			result := &nativeExecutionResult{Messages: []NativeMessage{{Role: "user", Content: "Run tests"}}}
			if err := r.save(x.Context, "tools", result); err != nil {
				t.Fatal(err)
			}
			if scenario == "checkpoint failure" {
				memory.beforeSave = func(*agentcore.NativeState) error { return fmt.Errorf("checkpoint unavailable") }
			} else {
				memory.afterSave = func() {
					run, _ := memory.GetRun(x.Context, x.AppID, x.Run.ID)
					run.Status = agentcore.RunStatusCancelled
					if err := memory.UpdateRun(x.Context, run); err != nil {
						t.Fatal(err)
					}
				}
			}
			calls := 0
			x.Tools.Register(tools.Definition{Name: "run_command", Mutating: true}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
				calls++
				return json.RawMessage(`{}`), nil
			})
			ctx := context.WithValue(x.Context, nativeCallRecorderKey{}, &nativeCallRecorder{recorder: r, result: result})
			for _, id := range []string{"first", "second"} {
				got := executeSingleNativeToolCall(ctx, x, NativeBlock{ToolCallID: id, ToolName: "run_command", Input: json.RawMessage(`{}`)})
				if !got.IsError {
					t.Fatal("launch fence missing")
				}
			}
			if calls != 0 {
				t.Fatal("external effect launched despite failed admission")
			}
		})
	}
}
