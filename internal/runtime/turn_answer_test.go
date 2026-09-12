package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

const diagramAnswer = "The editor supports **Mermaid**, `nwdiag`, and Excalidraw.\n\n| Format | Location |\n| --- | --- |\n| Mermaid | Diagram block |\n\nAdd a diagrams section to 4.2 and link to a dedicated guide."

func answerTestContext(t *testing.T, kind string) (*ExecutionContext, *testEventSink) {
	t.Helper()
	sink := &testEventSink{}
	return &ExecutionContext{
		Context: context.Background(), AppID: "app-a", Store: store.NewMemory(), EventSink: sink,
		Agent: &agentcore.Agent{Name: "Ask Agent", RuntimeKind: kind},
		Run: &agentcore.AgentRun{ID: "answer-run", AppID: "app-a", RuntimeKind: kind,
			Input: agentcore.RunInput{TurnPolicy: agentcore.TurnPolicy{
				Mode: agentcore.TurnPolicyPauseAfterAssist, CompletionMode: agentcore.TurnCompletionExplicit, MaxCompletionCorrections: 2,
			}},
		},
	}, sink
}

func executeAnswerTest(t *testing.T, x *ExecutionContext, preamble, answer, outcome string) (*Result, error) {
	t.Helper()
	input, err := json.Marshal(nativeFinishTurnRequest{Outcome: outcome, Summary: answer, Blocker: "The document must be shared with this agent."})
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeNativeModel{responses: []NativeModelResponse{{Message: NativeMessage{
		Role: "assistant", Content: preamble, Blocks: []NativeBlock{
			{Type: nativeBlockTypeText, Text: preamble},
			{Type: nativeBlockTypeToolCall, ToolCallID: "finish-1", ToolName: nativeToolFinishTurn, Input: input},
		},
	}}}}
	return NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: model}}).Execute(x)
}

func assertPublishedAnswer(t *testing.T, x *ExecutionContext, sink *testEventSink, result *Result, answer string) {
	t.Helper()
	if result == nil || result.AssistantMessage != answer || result.AssistantMessageID == "" || !result.MessagesPersisted {
		t.Fatalf("answer was not authoritative: %+v", result)
	}
	messages, err := x.Store.ListMessages(x.Context, x.Run.AppID, x.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, message := range messages {
		if message.RuntimeMessageID != result.AssistantMessageID {
			continue
		}
		found++
		if message.Content != answer || message.Role != "assistant" || message.MessageType != "assistant_final" {
			t.Fatalf("stored answer: %+v", message)
		}
		var blocks []NativeBlock
		if err := json.Unmarshal(message.ContentBlocks, &blocks); err != nil {
			t.Fatal(err)
		}
		if len(blocks) != 1 || blocks[0].Type != nativeBlockTypeText || blocks[0].Text != answer {
			t.Fatalf("answer blocks disagree: %+v", blocks)
		}
	}
	if found != 1 {
		t.Fatalf("expected one canonical message, got %d", found)
	}
	// The final completed-message event must contain the same answer and ID,
	// even when a separate preamble was already emitted and persisted.
	var final Event
	for _, event := range sink.events {
		if event.Type == "assistant_message_completed" {
			final = event
		}
	}
	if final.Data["message_id"] != result.AssistantMessageID || final.Data["content"] != answer || final.Data["message_type"] != "assistant_final" {
		t.Fatalf("last completed event does not contain the canonical answer: %+v", final)
	}
}

func TestAdaptersPublishAcceptedAnswerInsteadOfPreamble(t *testing.T) {
	cases := []struct{ name, preamble, answer string }{
		{"reported", "All the evidence is in. Here's the complete picture:", diagramAnswer},
		{"followup", "The investigation was already complete — here's the full answer:", diagramAnswer},
		{"reworded", "Here's the answer again, with that word removed:", diagramAnswer},
		{"tool only", "", diagramAnswer},
		{"duplicate prose", diagramAnswer, diagramAnswer},
		{"short", "Here is the answer:", "No."},
		{"multilingual", "Voici la réponse :", "Oui, Mermaid est disponible dans l’éditeur."},
		{"formerly rejected prefix", "", "I will is the future tense of I do."},
		{"formerly rejected punctuation", "", "The delimiter is:"},
	}
	for _, kind := range []string{agentcore.RuntimeNativeSDK} {
		for _, tt := range cases {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				x, sink := answerTestContext(t, kind)
				result, err := executeAnswerTest(t, x, tt.preamble, tt.answer, "completed")
				if err != nil {
					t.Fatal(err)
				}
				if !result.TurnFinished || result.CompletionCorrections != 0 {
					t.Fatalf("unexpected finish: %+v", result)
				}
				assertPublishedAnswer(t, x, sink, result, tt.answer)
			})
		}
	}
}

func TestAdaptersPublishBlockedAnswer(t *testing.T) {
	for _, kind := range []string{agentcore.RuntimeNativeSDK} {
		t.Run(kind, func(t *testing.T) {
			x, sink := answerTestContext(t, kind)
			result, err := executeAnswerTest(t, x, "I found a blocker.", "Share the document so I can inspect its diagrams section.", "blocked")
			if err != nil {
				t.Fatal(err)
			}
			if !result.AwaitingInput || result.TurnOutcome != "blocked" {
				t.Fatalf("expected input pause: %+v", result)
			}
			assertPublishedAnswer(t, x, sink, result, "Share the document so I can inspect its diagrams section.")
		})
	}
}

type failAnswerStore struct{ *store.Memory }

func (s failAnswerStore) AppendMessage(ctx context.Context, m *agentcore.AgentRunMessage) error {
	if m.Content == diagramAnswer {
		return errors.New("answer storage unavailable")
	}
	return s.Memory.AppendMessage(ctx, m)
}

func TestAdaptersDoNotAcceptAnswerPersistenceFailure(t *testing.T) {
	for _, kind := range []string{agentcore.RuntimeNativeSDK} {
		t.Run(kind, func(t *testing.T) {
			x, sink := answerTestContext(t, kind)
			x.Store = failAnswerStore{x.Store.(*store.Memory)}
			_, err := executeAnswerTest(t, x, "Here is the answer:", diagramAnswer, "completed")
			if err == nil || !strings.Contains(err.Error(), "persist turn answer") {
				t.Fatalf("expected storage failure, got %v", err)
			}
			for _, event := range sink.events {
				if event.Type == "assistant_message_completed" && event.Data["content"] == diagramAnswer {
					t.Fatal("published unpersisted answer")
				}
			}
		})
	}
}

func TestNativeRecoveryPublishesCanonicalAnswerWithoutModelCall(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "legacy"}[legacy], func(t *testing.T) {
			x := contextTestExec(t)
			sink := &testEventSink{}
			x.EventSink = sink
			r, err := openNativeRecorder(x.Context, x, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.initialMessages(true); err != nil {
				t.Fatal(err)
			}
			input, err := json.Marshal(nativeFinishTurnRequest{Outcome: "completed", Summary: diagramAnswer})
			if err != nil {
				t.Fatal(err)
			}
			finished := &nativeExecutionResult{
				TurnFinished: true, TurnOutcome: "completed", AssistantText: "Here is the answer:", AssistantMessageID: "preamble-1",
				FinalAnswer: newTurnAnswer(x, "finish-1", diagramAnswer),
				Messages: []NativeMessage{
					{Role: "assistant", Content: "Here is the answer:", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolName: nativeToolFinishTurn, ToolCallID: "finish-1", Input: input}}},
					{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolName: nativeToolFinishTurn, ToolCallID: "finish-1", Output: `{"accepted":true}`}}},
				},
			}
			if legacy {
				finished.FinalAnswer = nil
			}
			if err := r.save(x.Context, "tools", finished); err != nil {
				t.Fatal(err)
			}
			model := &contextTestModel{}
			adapter := NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: model}})
			first, err := adapter.Execute(x)
			if err != nil {
				t.Fatal(err)
			}
			assertPublishedAnswer(t, x, sink, first, diagramAnswer)
			replay, err := adapter.Execute(x)
			if err != nil {
				t.Fatal(err)
			}
			assertPublishedAnswer(t, x, sink, replay, diagramAnswer)
			if first.AssistantMessageID != replay.AssistantMessageID || len(model.requests) != 0 {
				t.Fatal("recovery restarted work or changed answer identity")
			}
		})
	}
}

func TestTurnAnswerIdentitySeparatesFollowupTurns(t *testing.T) {
	x, _ := answerTestContext(t, agentcore.RuntimeNativeSDK)
	first := newTurnAnswer(x, "finish-1", diagramAnswer)
	x.Run.Input.Metadata = map[string]any{"last_resume": map[string]any{"content": "What about Excalidraw?", "id": "resume-2"}}
	second := newTurnAnswer(x, "finish-1", diagramAnswer)
	if first.MessageID == second.MessageID {
		t.Fatal("follow-up answer reused previous turn ID")
	}
}

func TestNativeInvalidFinishContractContinuesWithoutPublishingAnswer(t *testing.T) {
	for _, input := range []string{
		`{"outcome":"completed","summary":""}`,
		`{"outcome":"completed","summary":123}`,
		`{"outcome":"unknown","summary":"Some text"}`,
		`{"outcome":"blocked","summary":"Some text"}`,
	} {
		t.Run(input, func(t *testing.T) {
			x, sink := answerTestContext(t, agentcore.RuntimeNativeSDK)
			valid, err := json.Marshal(nativeFinishTurnRequest{Outcome: "completed", Summary: diagramAnswer})
			if err != nil {
				t.Fatal(err)
			}
			model := &fakeNativeModel{responses: []NativeModelResponse{
				{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolName: nativeToolFinishTurn, ToolCallID: "invalid", Input: json.RawMessage(input)}}}},
				{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolName: nativeToolFinishTurn, ToolCallID: "valid", Input: valid}}}},
			}}
			result, err := NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: model}}).Execute(x)
			if err != nil {
				t.Fatal(err)
			}
			if len(model.requests) != 2 || result.CompletionCorrections != 1 {
				t.Fatalf("invalid finish did not continue work: %+v", result)
			}
			assertPublishedAnswer(t, x, sink, result, diagramAnswer)
		})
	}
}
