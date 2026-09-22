package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestNativeFreshWorkspaceInvalidatesApprovalAndCompletedTurn(t *testing.T) {
	x := contextTestExec(t)
	r, err := openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	approval, _ := json.Marshal(nativeApprovalBlockState{ApprovalRequired: true, InteractionID: "old-approval", ToolName: "run_command", ToolInputPreview: json.RawMessage(`{"command":"git push"}`)})
	r.state.ResumeKey = nativeResumeKey(x)
	r.state.Instructions = x.Run.Input.Instructions
	result := &nativeExecutionResult{TurnFinished: true, Usage: NativeUsage{InputTokens: 123}, Messages: []NativeMessage{
		{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolName: "external_api", ToolCallID: "completed-effect", Output: "Already delivered"}}},
		{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolName: "run_command", ToolCallID: "pending-effect", Output: string(approval)}}},
	}}
	if err := r.save(x.Context, "done", result); err != nil {
		t.Fatal(err)
	}
	x.Run.Input.Metadata = map[string]any{workspace.RecoveryMetadataKey: "replacement-1"}
	r, err = openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	messages, cached, err := r.initialMessages(false)
	if err != nil || cached != nil {
		t.Fatalf("fresh checkout must not reuse completed result: %v", err)
	}
	if len(nativeApprovalPlaceholders(messages)) != 0 {
		t.Fatal("stale approved commands could still execute")
	}
	if messages[0].Blocks[0].Output != "Already delivered" || !strings.Contains(messages[len(messages)-1].Content, "Unpushed edits") || r.state.Usage.InputTokens != 123 {
		t.Fatal("lost external effect, usage, or fresh-checkout notice")
	}
	if err := r.save(x.Context, "ready", &nativeExecutionResult{Messages: messages, Usage: r.state.Usage}); err != nil {
		t.Fatal(err)
	}
	r, _ = openNativeRecorder(x.Context, x, false)
	again, _, err := r.initialMessages(false)
	if err != nil || len(again) != len(messages) {
		t.Fatal("same recovery notice repeated after checkpoint reload")
	}
}

func TestNativeFreshWorkspaceNotifiesLegacySummary(t *testing.T) {
	x := contextTestExec(t)
	x.Run.OutputSummary = json.RawMessage(`{"native_messages":[{"role":"user","content":"old request"},{"role":"assistant","content":"old local edits"}]}`)
	x.Run.Input.Metadata = map[string]any{workspace.RecoveryMetadataKey: "replacement-legacy"}
	r, err := openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	messages, cached, err := r.initialMessages(false)
	if err != nil || cached != nil || !strings.Contains(messages[len(messages)-1].Content, "Unpushed edits") {
		t.Fatal("legacy summary omitted workspace-loss notice")
	}
}
