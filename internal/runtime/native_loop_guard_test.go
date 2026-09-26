package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestNativeAdapterStopsRepeatedNoProgressCalls(t *testing.T) {
	for _, tc := range []struct {
		name, toolName string
		toolError      error
	}{
		{name: "unchanged read", toolName: "read_files"},
		{name: "rejected no-op edit", toolName: "edit_file", toolError: errors.New("old_string and new_string must differ; no edit was made")},
	} {
		t.Run(tc.name, func(t *testing.T) { testNativeNoProgressStop(t, tc.toolName, tc.toolError) })
	}
}

func testNativeNoProgressStop(t *testing.T, toolName string, toolError error) {
	t.Helper()
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	registry.Register(tools.Definition{Name: toolName, Description: "test tool", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
			if toolError != nil {
				return nil, toolError
			}
			return json.RawMessage(`{"content":"unchanged"}`), nil
		})
	model := &fakeNativeModel{}
	for i := 0; i < 6; i++ {
		model.responses = append(model.responses, NativeModelResponse{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{
			Type: nativeBlockTypeToolCall, ToolCallID: "call-" + string(rune('1'+i)), ToolName: toolName, Input: json.RawMessage(`{"path":"same.go"}`),
		}}}})
	}
	run := &agentcore.AgentRun{ID: "run-loop", AppID: "app-a", Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"}, Input: agentcore.RunInput{Instructions: "inspect"}, RuntimeKind: agentcore.RuntimeNativeSDK}
	adapter := NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: model}, MaxToolSteps: 10})
	_, err := adapter.Execute(&ExecutionContext{Context: context.Background(), AppID: "app-a", Store: mem, Agent: &agentcore.Agent{Name: "Native", RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: []string{toolName}}, Run: run, Tools: registry, AllowedTools: map[string]bool{toolName: true}})
	if err == nil || !strings.Contains(err.Error(), "stopped run after 5 identical "+toolName+" calls") {
		t.Fatalf("expected no-progress stop, got %v", err)
	}
	if len(model.requests) != 5 {
		t.Fatalf("expected five model requests, got %d", len(model.requests))
	}
	for i, request := range model.requests {
		corrections := 0
		for _, message := range request.Messages {
			if message.Provenance == "runtime_correction" {
				corrections++
				if !strings.Contains(message.Content, "Do not repeat that call") {
					t.Fatalf("request %d has an ineffective correction: %q", i, message.Content)
				}
			}
		}
		if i < 3 && corrections != 0 || i >= 3 && corrections != 1 {
			t.Fatalf("request %d has %d recovery notices", i, corrections)
		}
	}
	calls, err := mem.ListToolCalls(context.Background(), "app-a", "run-loop")
	if err != nil || len(calls) != 5 {
		t.Fatalf("expected five recorded tool calls, got %d: %v", len(calls), err)
	}
}

func TestNativeAdapterContinuesAfterRecoveryChangesAction(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	registry.Register(tools.Definition{Name: "read_files", Description: "read files", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"content":"unchanged"}`), nil
		})
	model := &fakeNativeModel{}
	for i, path := range []string{"same.go", "same.go", "same.go", "other.go"} {
		input, _ := json.Marshal(map[string]string{"path": path})
		model.responses = append(model.responses, NativeModelResponse{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{
			Type: nativeBlockTypeToolCall, ToolCallID: "call-" + string(rune('1'+i)), ToolName: "read_files", Input: input,
		}}}})
	}
	model.responses = append(model.responses, NativeModelResponse{Message: NativeMessage{Role: "assistant", Content: "done"}})
	run := &agentcore.AgentRun{ID: "run-recovers", AppID: "app-a", Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"}, Input: agentcore.RunInput{Instructions: "inspect"}, RuntimeKind: agentcore.RuntimeNativeSDK}
	adapter := NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: model}, MaxToolSteps: 10})
	result, err := adapter.Execute(&ExecutionContext{Context: context.Background(), AppID: "app-a", Store: mem, Agent: &agentcore.Agent{Name: "Native", RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: []string{"read_files"}}, Run: run, Tools: registry, AllowedTools: map[string]bool{"read_files": true}})
	if err != nil || result == nil || result.AssistantMessage != "done" {
		t.Fatalf("expected recovery to complete, got result=%+v error=%v", result, err)
	}
	if len(model.requests) != 5 || model.requests[3].Messages[len(model.requests[3].Messages)-1].Provenance != "runtime_correction" {
		t.Fatal("model did not receive the recovery notice before changing action")
	}
}

func TestNativeNoProgressResetOnChangedRead(t *testing.T) {
	recorder := &nativeRecorder{}
	read := nativeExecutedToolCall{ToolName: "read_files", Input: json.RawMessage(`{"path":"same.go"}`), Output: "first"}
	for i := 1; i <= 2; i++ {
		count, stopped := recorder.observeNoProgress(read)
		if count != i || stopped {
			t.Fatalf("read %d: count=%d stopped=%t", i, count, stopped)
		}
	}
	read.Output = "updated"
	if count, stopped := recorder.observeNoProgress(read); count != 1 || stopped {
		t.Fatalf("changed output should start a fresh count: %d, %t", count, stopped)
	}
	read.Input = json.RawMessage(`{"path":"other.go"}`)
	if count, stopped := recorder.observeNoProgress(read); count != 1 || stopped {
		t.Fatalf("changed input should start a fresh count: %d, %t", count, stopped)
	}
}

func TestNativeNoProgressRecoverySurvivesCheckpoint(t *testing.T) {
	recorder := &nativeRecorder{}
	read := nativeExecutedToolCall{ToolName: "read_files", Input: json.RawMessage(`{"path":"same.go"}`), Output: "unchanged"}
	for i := 0; i < nativeNoProgressRecoveryLimit; i++ {
		recorder.observeNoProgress(read)
	}
	recorder.state.NoProgress.RecoverySent = true
	encoded, err := json.Marshal(recorder.state)
	if err != nil {
		t.Fatal(err)
	}
	var restored nativeCheckpoint
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	recorder.state = restored
	if count, stopped := recorder.observeNoProgress(read); count != 4 || stopped || !recorder.state.NoProgress.RecoverySent {
		t.Fatalf("recovery was lost after restart: count=%d stopped=%t state=%+v", count, stopped, recorder.state.NoProgress)
	}
	if count, stopped := recorder.observeNoProgress(read); count != 5 || !stopped {
		t.Fatalf("restarted guard failed to stop repeated call: count=%d stopped=%t", count, stopped)
	}
}
