package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestFinishTurnPreservesStandaloneSpacesInStreamedSummary(t *testing.T) {
	const summary = "## Direct traffic\n\nFor **Sep 1–9, 2026**, Direct has brought **981 unique visitors** across **1,375 sessions**.\n\n| Date | Visitors |\n| --- | ---: |\n| Sep 1 | 186 |\n\n- Sep 8 is only **7% lower**. See [report](https://example.com/report) and `direct_traffic`."
	payload, err := json.Marshal(map[string]string{"outcome": "completed", "summary": summary})
	if err != nil {
		t.Fatal(err)
	}
	// Numeric tokens often follow a standalone space. Exercise that boundary
	// through each provider adapter and both live and final argument assembly.
	var fragments []string
	for i, part := range strings.Split(string(payload), " ") {
		if i > 0 {
			fragments = append(fragments, " ")
		}
		fragments = append(fragments, part)
	}
	fragments = append(fragments, "") // Empty terminal chunks must remain harmless.

	for _, provider := range []string{"native", "eino", "agentic"} {
		t.Run(provider, func(t *testing.T) {
			var chunks []NativeModelResponse
			for _, fragment := range fragments {
				var message NativeMessage
				switch provider {
				case "native":
					message = NativeMessage{Role: "assistant", Blocks: []NativeBlock{{
						Type: nativeBlockTypeToolCall, ToolCallID: "finish-1",
						ToolName: "finish_turn", Input: json.RawMessage(fragment),
					}}}
				case "eino":
					message = einoMessageChunkToNative(&schema.Message{
						Role: schema.Assistant,
						ToolCalls: []schema.ToolCall{{
							ID: "finish-1", Type: "function",
							Function: schema.FunctionCall{Name: "finish_turn", Arguments: fragment},
						}},
					}, nativeToolNameMapper{})
				case "agentic":
					message = agenticMessageChunkToNative(&schema.AgenticMessage{
						Role: schema.AgenticRoleTypeAssistant,
						ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{
							CallID: "finish-1", Name: "finish_turn", Arguments: fragment,
						})},
					}, nativeToolNameMapper{})
				}
				chunks = append(chunks, NativeModelResponse{Message: message})
			}
			sink := &testEventSink{}
			mem := store.NewMemory()
			adapter := NewNativeAdapterWithConfig(NativeConfig{
				ModelFactory: fakeNativeFactory{model: &fakeStreamingNativeModel{chunks: chunks}},
				MaxToolSteps: 2,
			})
			result, err := adapter.Execute(&ExecutionContext{
				Context: context.Background(), AppID: "usermaven", Store: mem,
				Agent: &agentcore.Agent{Name: "Maven", RuntimeKind: agentcore.RuntimeNativeSDK},
				Run: &agentcore.AgentRun{
					ID: "finish-spaces", AppID: "usermaven", RuntimeKind: agentcore.RuntimeNativeSDK,
					Input: agentcore.RunInput{TurnPolicy: agentcore.TurnPolicy{
						Mode:                     agentcore.TurnPolicyPauseAfterAssist,
						CompletionMode:           agentcore.TurnCompletionExplicit,
						MaxCompletionCorrections: 2,
					}},
				},
				EventSink: sink,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.TurnFinished || result.TurnOutcome != "completed" || result.CompletionCorrections != 0 {
				t.Fatalf("unexpected completion result: %#v", result)
			}
			if result.AssistantMessage != summary {
				t.Errorf("completed answer lost whitespace: %q", result.AssistantMessage)
			}
			calls, err := mem.ListToolCalls(context.Background(), "usermaven", "finish-spaces")
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || calls[0].ToolName != "finish_turn" {
				t.Fatalf("expected one finish_turn call, got %#v", calls)
			}
			var final struct {
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal(calls[0].Input, &final); err != nil {
				t.Fatal(err)
			}
			if final.Summary != summary {
				t.Errorf("final summary lost whitespace:\n got: %q\nwant: %q", final.Summary, summary)
			}
			// Usermaven renders the accepted summary from this persisted envelope.
			var envelope struct {
				Output string `json:"output"`
			}
			if err := json.Unmarshal(calls[0].Output, &envelope); err != nil {
				t.Fatal(err)
			}
			var accepted struct {
				Accepted bool   `json:"accepted"`
				Summary  string `json:"summary"`
			}
			if err := json.Unmarshal([]byte(envelope.Output), &accepted); err != nil {
				t.Fatal(err)
			}
			if !accepted.Accepted || accepted.Summary != summary {
				t.Errorf("persisted completion lost whitespace: %#v", accepted)
			}
			var streamedArgs string
			for _, event := range sink.events {
				if event.Type == "tool_call_args_delta" {
					streamedArgs = event.Data["args_text"].(string)
				}
			}
			if streamedArgs != string(payload) {
				t.Errorf("streamed arguments lost whitespace:\n got: %q\nwant: %q", streamedArgs, payload)
			}
		})
	}
}
