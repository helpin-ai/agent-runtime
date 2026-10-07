package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/memory"
)

type nativeMemoryModels struct{}

func (nativeMemoryModels) ModelID() string { return "native-memory-test" }
func (nativeMemoryModels) Embed(_ context.Context, texts []string) ([][]float32, error) {
	v := make([][]float32, len(texts))
	for i := range texts {
		v[i] = []float32{1, 0}
	}
	return v, nil
}
func (nativeMemoryModels) Extract(_ context.Context, r memory.RetainRequest) ([]memory.ExtractedFact, error) {
	return []memory.ExtractedFact{{Text: r.Content, Type: "world", Entities: []string{"user"}}}, nil
}

func TestNativeMemorySurvivesAcrossRunsAndDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.db")
	open := func() *memory.SQLite {
		s, err := memory.OpenSQLite(ctx, memory.SQLiteConfig{Path: path, Extractor: nativeMemoryModels{}, Embedder: nativeMemoryModels{}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	backend := open()
	registry := tools.NewRegistry()
	tools.RegisterMemory(registry, backend)
	st := store.NewMemory()
	agent := &agentcore.Agent{ID: "personal-agent", AppID: "desktop", Name: "Personal", RuntimeKind: agentcore.RuntimeNativeSDK, ApprovalMode: agentcore.ApprovalModeNever, AllowedTools: []string{"memory_retain", "memory_recall"}, ExecutionConfig: json.RawMessage(`{"memory":{"enabled":true,"bank_id":"user-a"}}`)}
	execute := func(runID, tool, input string) *fakeNativeModel {
		model := &fakeNativeModel{responses: []NativeModelResponse{
			{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "call-" + runID, ToolName: tool, Input: json.RawMessage(input)}}}},
			{Message: NativeMessage{Role: "assistant", Content: "done"}},
		}}
		adapter := NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: model}, MaxToolSteps: 4})
		run := &agentcore.AgentRun{ID: runID, AppID: "desktop", AgentID: agent.ID, RuntimeKind: agentcore.RuntimeNativeSDK, Target: agentcore.TargetRef{Type: "chat", ID: runID}, Input: agentcore.RunInput{Instructions: "Use memory"}}
		result, err := adapter.Execute(&ExecutionContext{Context: ctx, AppID: "desktop", Agent: agent, Run: run, Store: st, Tools: registry, AllowedTools: map[string]bool{"memory_retain": true, "memory_recall": true}})
		if err != nil {
			t.Fatal(err)
		}
		if result.AssistantMessage != "done" {
			t.Fatalf("unexpected answer: %+v", result)
		}
		calls, err := st.ListToolCalls(ctx, "desktop", runID)
		if err != nil || len(calls) != 1 || calls[0].Error != "" {
			t.Fatalf("memory tool was not executed: %+v %v", calls, err)
		}
		return model
	}
	execute("first-run", "memory_retain", `{"document_id":"user-preference","content":"User prefers Go for backend services"}`)
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend = open()
	defer backend.Close()
	tools.RegisterMemory(registry, backend)
	model := execute("second-run", "memory_recall", `{"query":"Which language does the user prefer?"}`)
	if len(model.requests) != 2 {
		t.Fatalf("model rounds: %d", len(model.requests))
	}
	data, _ := json.Marshal(model.requests[1].Messages)
	if !strings.Contains(string(data), "User prefers Go for backend services") {
		t.Fatalf("recalled evidence did not reach the next model round: %s", data)
	}
}
