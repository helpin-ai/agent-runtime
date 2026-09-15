package runtime

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Explicit opt-in: a real compatible local server, not a protocol fixture.
func TestCompatibleLocalLiveToolAndRestoredTranscript(t *testing.T) {
	base := os.Getenv("NATIVE_COMPATIBLE_SMOKE_BASE_URL")
	if base == "" {
		t.Skip("set NATIVE_COMPATIBLE_SMOKE_BASE_URL for a real local model")
	}
	modelName := os.Getenv("NATIVE_COMPATIBLE_SMOKE_MODEL")
	if modelName == "" {
		t.Fatal("NATIVE_COMPATIBLE_SMOKE_MODEL is required")
	}
	_, x := compatibleTestModel(t, base, "none")
	x.Run.Input.Model.Model = modelName
	definition := tools.Definition{Name: "lookup_code", Description: "Look up the secret test code. Call this tool before answering the user.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	model, err := (EinoProviderFactory{MaxTokens: 512}).ResolveNativeModel(ctx, x, []tools.Definition{definition})
	if err != nil {
		t.Fatal(err)
	}
	request := NativeModelRequest{SystemPrompt: "Use lookup_code to find the secret test code. Never guess. After the tool result, answer with that code.", Messages: []NativeMessage{{Role: "user", Content: "What is the secret test code? Call lookup_code first."}}}
	first := readCompatibleStream(t, ctx, model, request)
	calls := nativeRawToolCallBlocks(first.Message)
	if len(calls) != 1 || calls[0].ToolName != "lookup_code" || calls[0].ToolCallID == "" || !json.Valid(calls[0].Input) {
		t.Fatalf("local model did not produce a valid tool call: %+v", first.Message)
	}
	request.Messages = append(request.Messages, first.Message, NativeMessage{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolName: "lookup_code", ToolCallID: calls[0].ToolCallID, Output: `{"code":"7429"}`}}})
	// Serialize the durable transcript and reconstruct the provider adapter.
	saved, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var restored NativeModelRequest
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	model, err = (EinoProviderFactory{MaxTokens: 512}).ResolveNativeModel(ctx, x, []tools.Definition{definition})
	if err != nil {
		t.Fatal(err)
	}
	second := readCompatibleStream(t, ctx, model, restored)
	if len(nativeRawToolCallBlocks(second.Message)) != 0 || !strings.Contains(second.Message.Content, "7429") {
		t.Fatalf("tool result was not continued: %+v", second.Message)
	}
	if first.Usage.InputTokens <= 0 || second.Usage.OutputTokens <= 0 {
		t.Fatal("local provider omitted usage")
	}
	t.Logf("model=%s no_auth=true tool_calls=1 restored_transcript=true input_tokens=%d output_tokens=%d", modelName, first.Usage.InputTokens+second.Usage.InputTokens, first.Usage.OutputTokens+second.Usage.OutputTokens)
}
