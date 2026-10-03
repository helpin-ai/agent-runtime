package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/memory"
)

type recordingMemory struct {
	scope memory.Scope
	doc   memory.RetainRequest
	calls int
}

func (m *recordingMemory) Retain(_ context.Context, s memory.Scope, r memory.RetainRequest) (memory.RetainResult, error) {
	m.scope = s
	m.doc = r
	m.calls++
	return memory.RetainResult{DocumentID: r.DocumentID}, nil
}
func (m *recordingMemory) Recall(_ context.Context, s memory.Scope, _ memory.RecallRequest) (memory.RecallResult, error) {
	m.scope = s
	m.calls++
	return memory.RecallResult{Results: []memory.Result{}}, nil
}
func (m *recordingMemory) Forget(_ context.Context, s memory.Scope, _ string) error {
	m.scope = s
	m.calls++
	return nil
}

func TestMemoryToolsRequireHostScopeAndRejectModelScope(t *testing.T) {
	m := &recordingMemory{}
	r := NewRegistry()
	RegisterMemory(r, m)
	call := CallContext{AppID: "app", RunID: "run", Agent: &agentcore.Agent{ID: "agent", AppID: "app", ExecutionConfig: json.RawMessage(`{"memory":{"enabled":true,"bank_id":"fixed"}}`)}, Run: &agentcore.AgentRun{AppID: "app", Input: agentcore.RunInput{Metadata: map[string]any{"memory_bank_id": "override"}}}}
	if _, err := r.Execute(context.Background(), call, "memory_retain", json.RawMessage(`{"document_id":"source","content":"Alice prefers Go"}`)); err != nil {
		t.Fatal(err)
	}
	if m.scope != (memory.Scope{AppID: "app", BankID: "fixed"}) || m.doc.Metadata["run_id"] != "run" || m.doc.Metadata["agent_id"] != "agent" {
		t.Fatalf("untrusted scope/provenance: %+v", m)
	}
	for _, input := range []string{`{"query":"Alice","bank_id":"victim"}`, `{"query":"Alice","app_id":"other"}`, `null`, `{"query":"Alice"} {}`} {
		if _, err := r.Execute(context.Background(), call, "memory_recall", json.RawMessage(input)); err == nil {
			t.Fatalf("accepted scope/input: %s", input)
		}
	}
	call.Agent.ExecutionConfig = json.RawMessage(`{"memory":{"enabled":true}}`)
	if _, err := r.Execute(context.Background(), call, "memory_recall", json.RawMessage(`{"query":"Alice"}`)); err != nil {
		t.Fatal(err)
	}
	if m.scope.BankID != "override" {
		t.Fatal("host dynamic scope ignored")
	}
	call.Run.Input.Metadata = nil
	before := m.calls
	if _, err := r.Execute(context.Background(), call, "memory_recall", json.RawMessage(`{"query":"Alice"}`)); err == nil {
		t.Fatal("missing bank accepted")
	}
	call.Agent.ExecutionConfig = json.RawMessage(`{}`)
	if _, err := r.Execute(context.Background(), call, "memory_recall", json.RawMessage(`{"query":"Alice"}`)); err == nil {
		t.Fatal("memory enabled implicitly")
	}
	call.Agent.ExecutionConfig = json.RawMessage(`{"memory":{"enabled":true,"bank_id":"fixed"}}`)
	call.Run.AppID = "other"
	if _, err := r.Execute(context.Background(), call, "memory_recall", json.RawMessage(`{"query":"Alice"}`)); err == nil {
		t.Fatal("cross-app run accepted")
	}
	if m.calls != before {
		t.Fatal("unauthorized request reached backend")
	}
	for _, name := range []string{"memory_retain", "memory_recall", "memory_forget"} {
		d, ok := r.Definition(name)
		if !ok {
			t.Fatalf("missing %s", name)
		}
		switch name {
		case "memory_retain":
			if d.EffectiveRiskLevel() != RiskLevelRoutine {
				t.Fatal("retain risk")
			}
		case "memory_recall":
			if d.EffectiveRiskLevel() != RiskLevelRead {
				t.Fatal("recall risk")
			}
		case "memory_forget":
			if d.EffectiveRiskLevel() != RiskLevelDestructive {
				t.Fatal("forget risk")
			}
		}
	}
}
