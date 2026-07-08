package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// registerMutatingCounter registers a mutating tool that counts executions.
func registerMutatingCounter(registry *tools.Registry, name string, counter *int32) {
	registry.Register(tools.Definition{
		Name:        name,
		Description: "mutating tool",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Mutating:    true,
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		atomic.AddInt32(counter, 1)
		return json.RawMessage(`{"document_id":"doc-123","url":"/docs/doc-123"}`), nil
	})
}

// pauseForApproval runs a first pass with an approval-mode agent that emits a
// single mutating tool call, and returns the paused run's OutputSummary plus
// the created interaction ID.
func pauseForApproval(t *testing.T, mem agentcore.Store, registry *tools.Registry, runID, toolName string) (json.RawMessage, string) {
	t.Helper()
	model := &fakeNativeModel{responses: []NativeModelResponse{{
		Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
			{Type: nativeBlockTypeText, Text: "Creating the document."},
			{Type: nativeBlockTypeToolCall, ToolCallID: "tc-1", ToolName: toolName, Input: json.RawMessage(`{"title":"Report"}`)},
		}},
	}}}
	run := &agentcore.AgentRun{
		ID: runID, AppID: "app-a", RuntimeKind: agentcore.RuntimeNativeSDK,
		Target: agentcore.TargetRef{Type: "workspace", ID: "W-1"},
		Input:  agentcore.RunInput{Instructions: "write a report"},
	}
	if err := mem.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	result, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: model}, MaxToolSteps: 3,
	}).Execute(&ExecutionContext{
		Context: context.Background(), AppID: "app-a", Store: mem,
		Agent: &agentcore.Agent{Name: "Native", RuntimeKind: agentcore.RuntimeNativeSDK, ApprovalMode: agentcore.ApprovalModeAlways, AllowedTools: []string{toolName}},
		Run:   run, Tools: registry, AllowedTools: map[string]bool{toolName: true},
	})
	if err != nil {
		t.Fatalf("pass 1 execute: %v", err)
	}
	if !result.WaitForApproval {
		t.Fatalf("expected pass 1 to pause for approval, got %#v", result)
	}
	interactions, err := mem.ListInteractions(context.Background(), "app-a", runID)
	if err != nil || len(interactions) != 1 {
		t.Fatalf("expected one interaction, got %v err=%v", interactions, err)
	}
	return result.OutputSummary, interactions[0].ID
}

// resolveInteraction marks an interaction resolved with a decision payload,
// mirroring engine.resolvePendingInteraction.
func resolveInteraction(t *testing.T, mem agentcore.Store, appID, runID, interactionID, decision, content string) {
	t.Helper()
	interactions, _ := mem.ListInteractions(context.Background(), appID, runID)
	for i := range interactions {
		if interactions[i].ID != interactionID {
			continue
		}
		interactions[i].Status = "resolved"
		interactions[i].ResponsePayload = json.RawMessage(`{"decision":"` + decision + `","content":` + strconvQuote(content) + `}`)
		if err := mem.UpdateInteraction(context.Background(), &interactions[i]); err != nil {
			t.Fatalf("resolve interaction: %v", err)
		}
		return
	}
	t.Fatalf("interaction %s not found", interactionID)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// resumeRun builds a resumed ExecutionContext carrying the paused OutputSummary
// and a last_resume intent, and runs another pass with the given model.
func resumeRun(mem agentcore.Store, registry *tools.Registry, runID string, summary json.RawMessage, intent string, model *fakeNativeModel) (*Result, *agentcore.AgentRun, error) {
	run := &agentcore.AgentRun{
		ID: runID, AppID: "app-a", RuntimeKind: agentcore.RuntimeNativeSDK,
		Target:        agentcore.TargetRef{Type: "workspace", ID: "W-1"},
		Input:         agentcore.RunInput{Instructions: "write a report", Metadata: map[string]any{"last_resume": map[string]any{"intent": intent}}},
		OutputSummary: summary,
	}
	result, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: model}, MaxToolSteps: 3,
	}).Execute(&ExecutionContext{
		Context: context.Background(), AppID: "app-a", Store: mem,
		Agent: &agentcore.Agent{Name: "Native", RuntimeKind: agentcore.RuntimeNativeSDK, ApprovalMode: agentcore.ApprovalModeAlways, AllowedTools: []string{"create_document"}},
		Run:   run, Tools: registry, AllowedTools: map[string]bool{"create_document": true},
	})
	return result, run, err
}

func TestNativeApprovedToolExecutesOnceOnResumeAndReplays(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	var execCount int32
	registerMutatingCounter(registry, "create_document", &execCount)

	// Pass 1: pauses for approval, tool NOT executed.
	summary, interactionID := pauseForApproval(t, mem, registry, "run-approve", "create_document")
	if atomic.LoadInt32(&execCount) != 0 {
		t.Fatalf("tool must not execute before approval, count=%d", execCount)
	}
	if !strings.Contains(string(summary), "approval_required") {
		t.Fatalf("expected paused transcript to hold approval_required placeholder")
	}

	// Approve the interaction, then resume.
	resolveInteraction(t, mem, "app-a", "run-approve", interactionID, "approve", "")
	model2 := &fakeNativeModel{responses: []NativeModelResponse{{Message: NativeMessage{Role: "assistant", Content: "done"}}}}
	result2, run2, err := resumeRun(mem, registry, "run-approve", summary, "approve", model2)
	if err != nil {
		t.Fatalf("resume execute: %v", err)
	}
	if atomic.LoadInt32(&execCount) != 1 {
		t.Fatalf("approved tool must execute exactly once, count=%d", execCount)
	}
	// The model on resume must have seen the REAL result, not approval_required.
	if len(model2.requests) == 0 {
		t.Fatalf("expected the model to be called after reconcile")
	}
	sawReal, sawPlaceholder := false, false
	for _, m := range model2.requests[0].Messages {
		for _, b := range m.Blocks {
			if b.Type == nativeBlockTypeToolResult && b.ToolCallID == "tc-1" {
				if strings.Contains(b.Output, "doc-123") {
					sawReal = true
				}
				if strings.Contains(b.Output, "approval_required") {
					sawPlaceholder = true
				}
			}
		}
	}
	if !sawReal || sawPlaceholder {
		t.Fatalf("model must see real result and no placeholder (real=%v placeholder=%v)", sawReal, sawPlaceholder)
	}
	// No new interaction created on resume.
	interactions, _ := mem.ListInteractions(context.Background(), "app-a", "run-approve")
	if len(interactions) != 1 {
		t.Fatalf("resume must not create a new approval interaction, got %d", len(interactions))
	}
	_ = result2

	// Pass 3 (replay): reuse the reconciled OutputSummary; tool must NOT run again.
	model3 := &fakeNativeModel{responses: []NativeModelResponse{{Message: NativeMessage{Role: "assistant", Content: "done again"}}}}
	if _, _, err := resumeRun(mem, registry, "run-approve", run2.OutputSummary, "approve", model3); err != nil {
		t.Fatalf("replay execute: %v", err)
	}
	if atomic.LoadInt32(&execCount) != 1 {
		t.Fatalf("branch-3 replay must not re-execute, count=%d", execCount)
	}
}

func TestNativeRequestChangesDoesNotExecute(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	var execCount int32
	registerMutatingCounter(registry, "create_document", &execCount)

	summary, interactionID := pauseForApproval(t, mem, registry, "run-changes", "create_document")
	resolveInteraction(t, mem, "app-a", "run-changes", interactionID, "request_changes", "add a summary section")

	model2 := &fakeNativeModel{responses: []NativeModelResponse{{Message: NativeMessage{Role: "assistant", Content: "revising"}}}}
	if _, _, err := resumeRun(mem, registry, "run-changes", summary, "request_changes", model2); err != nil {
		t.Fatalf("resume execute: %v", err)
	}
	if atomic.LoadInt32(&execCount) != 0 {
		t.Fatalf("request_changes must not execute the tool, count=%d", execCount)
	}
	sawFeedback := false
	for _, m := range model2.requests[0].Messages {
		for _, b := range m.Blocks {
			if b.Type == nativeBlockTypeToolResult && strings.Contains(b.Output, "add a summary section") {
				sawFeedback = true
			}
		}
	}
	if !sawFeedback {
		t.Fatalf("model must see the reviewer feedback as the tool result")
	}
}

func TestNativeFreshMutatingCallStillGatesAfterResume(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	var execCount int32
	registerMutatingCounter(registry, "create_document", &execCount)

	summary, interactionID := pauseForApproval(t, mem, registry, "run-fresh", "create_document")
	resolveInteraction(t, mem, "app-a", "run-fresh", interactionID, "approve", "")

	// On resume, after the approved call executes, the model emits a NEW
	// mutating call (new tool_call_id) — it must gate on its own, not auto-run.
	model2 := &fakeNativeModel{responses: []NativeModelResponse{{
		Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
			{Type: nativeBlockTypeText, Text: "Creating a second document."},
			{Type: nativeBlockTypeToolCall, ToolCallID: "tc-2", ToolName: "create_document", Input: json.RawMessage(`{"title":"Second"}`)},
		}},
	}}}
	result2, _, err := resumeRun(mem, registry, "run-fresh", summary, "approve", model2)
	if err != nil {
		t.Fatalf("resume execute: %v", err)
	}
	if atomic.LoadInt32(&execCount) != 1 {
		t.Fatalf("only the first approved call runs; the fresh call must gate, count=%d", execCount)
	}
	if !result2.WaitForApproval {
		t.Fatalf("fresh mutating call must pause for its own approval")
	}
	interactions, _ := mem.ListInteractions(context.Background(), "app-a", "run-fresh")
	if len(interactions) != 2 {
		t.Fatalf("expected a second approval interaction for the fresh call, got %d", len(interactions))
	}
}
