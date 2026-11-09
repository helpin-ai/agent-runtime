package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestNativeMCPAuthStopsBeforeLaterMutation(t *testing.T) {
	x := contextTestExec(t)
	x.Agent.ExecutionConfig = nil
	writes := 0
	x.Tools.Register(tools.Definition{Name: "expired_read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		return nil, &mcp.AuthenticationError{ServerID: "crm", Reason: "unauthorized"}
	})
	x.Tools.Register(tools.Definition{Name: "write_crm", Mutating: true, InputSchema: map[string]any{"type": "object"}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		writes++
		return json.RawMessage(`{}`), nil
	})
	x.AllowedTools = map[string]bool{"expired_read": true, "write_crm": true}
	m := &fakeNativeModel{responses: []NativeModelResponse{{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "a", ToolName: "expired_read", Input: json.RawMessage(`{}`)}, {Type: nativeBlockTypeToolCall, ToolCallID: "b", ToolName: "write_crm", Input: json.RawMessage(`{}`)}}}}}}
	_, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}})
	var auth *mcp.AuthenticationError
	if !errors.As(err, &auth) || writes != 0 || len(m.requests) != 1 {
		t.Fatalf("auth must interrupt: err=%v writes=%d requests=%d", err, writes, len(m.requests))
	}
	r, err := openNativeRecorder(x.Context, x, false)
	if err != nil {
		t.Fatal(err)
	}
	messages, _, err := r.initialMessages(false)
	if err != nil {
		t.Fatal(err)
	}
	assertCompleteToolGroups(t, messages)
	if !strings.Contains(messages[len(messages)-1].Blocks[0].Output, "not started") {
		t.Fatalf("lost non-execution evidence: %+v", messages)
	}
}

func TestNativeSavedResultRangesAndRunIsolation(t *testing.T) {
	x := contextTestExec(t)
	full := strings.Repeat("å界", 6000)
	if err := recordNativeToolCall(x.Context, x, nativeExecutedToolCall{ToolCallID: "receipt", ToolName: "write_crm", Output: full, Mutating: true}, "saved", ""); err != nil {
		t.Fatal(err)
	}
	raw, err := nativeSavedToolResult(x.Context, x, json.RawMessage(`{"tool_call_id":"receipt","offset":5000,"limit":17}`))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Content string `json:"content"`
		Next    int    `json:"next_offset"`
	}
	json.Unmarshal([]byte(raw), &page)
	if page.Content != string([]rune(full)[5000:5017]) || page.Next != 5017 {
		t.Fatalf("bad range: %s", raw)
	}
	x.Run.ID = "different"
	if _, err := nativeSavedToolResult(x.Context, x, json.RawMessage(`{"tool_call_id":"receipt"}`)); err == nil {
		t.Fatal("cross-run output disclosed")
	}
	x.Run.ID = "r"
	x.AppID = "different"
	if _, err := nativeSavedToolResult(x.Context, x, json.RawMessage(`{"tool_call_id":"receipt"}`)); err == nil {
		t.Fatal("cross-app output disclosed")
	}
}

func TestNativeCompactionProtectedFloorDoesNotBuySummary(t *testing.T) {
	x := contextTestExec(t)
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	r.initialMessages(true)
	result := &nativeExecutionResult{Messages: []NativeMessage{{Role: "user", Content: strings.Repeat("required instructions ", 2000)}, {Role: "assistant", Content: "recent"}}}
	m := &contextTestModel{}
	p, _ := nativeContextPolicy(x)
	if compacted, err := nativeCompact(x.Context, r, m, p, "", nil, result, true); err == nil || compacted || m.summaries != 0 {
		t.Fatalf("paid for impossible compaction: %v %d", err, m.summaries)
	}
}

func TestNativeNoProgressDetectsResearchCycles(t *testing.T) {
	r := &nativeRecorder{}
	for i := 0; i < 10; i++ {
		name := []string{"mcp_describe_crm_fields", "web_search"}[i%2]
		count, stop := r.observeNoProgress(nativeExecutedToolCall{ToolName: name, Input: json.RawMessage(`{}`), Output: "unchanged"})
		if i == 9 && (!stop || count != 5) {
			t.Fatalf("cycle not detected: %d %v", count, stop)
		}
	}
	if _, stop := r.observeNoProgress(nativeExecutedToolCall{ToolName: "poll_status", Output: "running"}); stop || r.state.NoProgress != nil {
		t.Fatal("poll must reset research guard")
	}
}

func TestNativeUsageAnchorPersistsAndInvalidatesPricedPrefix(t *testing.T) {
	x := contextTestExec(t)
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	messages := []NativeMessage{{Role: "user", Content: strings.Repeat("text ", 100)}}
	defs := nativeAllowedToolDefinitions(x)
	r.anchorInput("policy", messages, defs, 80)
	if err := r.save(x.Context, "ready", &nativeExecutionResult{Messages: messages}); err != nil {
		t.Fatal(err)
	}
	loaded, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.adjustedInput("policy", messages, defs); got != 80 {
		t.Fatalf("provider usage not restored: %d", got)
	}
	grown := append(append([]NativeMessage{}, messages...), NativeMessage{Role: "assistant", Content: "progress"})
	want := 80 + nativeRequestTokens("policy", grown, defs) - nativeRequestTokens("policy", messages, defs)
	if got := loaded.adjustedInput("policy", grown, defs); got != want {
		t.Fatalf("append delta: %d != %d", got, want)
	}
	messages[0].Content = "changed user requirement"
	if got := loaded.adjustedInput("policy", messages, defs); got != nativeRequestTokens("policy", messages, defs) || loaded.state.InputAnchor != nil {
		t.Fatal("rewritten history retained stale usage anchor")
	}
	for _, change := range []string{"system", "model", "schema"} {
		t.Run(change, func(t *testing.T) {
			loaded.anchorInput("policy", messages, defs, 80)
			system := "policy"
			changedDefs := append([]tools.Definition{}, defs...)
			switch change {
			case "system":
				system = "new policy"
			case "model":
				x.Agent.Model = "another model"
			case "schema":
				changedDefs = append(changedDefs, tools.Definition{Name: "new_tool"})
			}
			loaded.adjustedInput(system, messages, changedDefs)
			if loaded.state.InputAnchor != nil {
				t.Fatal("stale anchor survived identity change")
			}
		})
	}
}

func TestNativeResultBudgetPreservesReceiptsAndOriginalOutputs(t *testing.T) {
	x := contextTestExec(t)
	names := newNativeToolNameMapper(nativeAllowedToolDefinitions(x))
	large := strings.Repeat("source evidence ", 2000)
	messages := []NativeMessage{{Role: "assistant"}}
	for i := 0; i < 12; i++ {
		messages = append(messages, NativeMessage{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: fmt.Sprint(i), ToolName: "fetch", Output: large}}})
	}
	bounded := nativeBudgetToolResults(messages, names)
	total := 0
	for i := 1; i < len(bounded); i++ {
		total += len([]rune(bounded[i].Blocks[0].Output))
		if !strings.Contains(bounded[i].Blocks[0].Output, "read_tool_result") || messages[i].Blocks[0].Output != large {
			t.Fatal("lost retrieval reference or original output")
		}
	}
	if total > 16000 {
		t.Fatalf("parallel output exceeds budget: %d", total)
	}
	receipt := NativeMessage{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: "receipt", ToolName: "submit_item_result", Output: `{"status":"accepted","item_id":"exact-item"}`}}}
	out := nativeBudgetToolResults([]NativeMessage{{Role: "assistant"}, receipt, {Role: "assistant", Content: "done"}}, names)
	if out[1].Blocks[0].Output != receipt.Blocks[0].Output {
		t.Fatal("receipt altered")
	}
}

func TestNativeCompactionCooldownSurvivesRestart(t *testing.T) {
	x := contextTestExec(t)
	x.Agent.SystemPrompt = strings.Repeat("fixed policy ", 2000)
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	messages, _, err := r.initialMessages(true)
	if err != nil {
		t.Fatal(err)
	}
	messages = append(messages, NativeMessage{Role: "assistant", Content: strings.Repeat("old research ", 300)})
	r.state.NextCompactionTokens = 25000
	if err := r.save(x.Context, "ready", &nativeExecutionResult{Messages: messages}); err != nil {
		t.Fatal(err)
	}
	model := &contextTestModel{}
	if _, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: model}}); err != nil {
		t.Fatal(err)
	}
	if model.summaries != 0 || len(model.requests) != 1 {
		t.Fatalf("restart reset cooldown: summaries=%d requests=%d", model.summaries, len(model.requests))
	}
}

func TestNativeUsageAnchorInvalidatesWhenConsumedOutputShrinks(t *testing.T) {
	x := contextTestExec(t)
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	defs := nativeAllowedToolDefinitions(x)
	messages := []NativeMessage{{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "source", ToolName: "fetch", Input: json.RawMessage(`{}`)}}}, {Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: "source", ToolName: "fetch", Output: strings.Repeat("evidence ", 2000)}}}}
	r.anchorInput("policy", messages, defs, 400)
	messages = append(messages, NativeMessage{Role: "assistant", Content: "Evidence consumed"})
	r.adjustedInput("policy", messages, defs)
	if r.state.InputAnchor != nil {
		t.Fatal("priced prefix changed after output reduction but anchor survived")
	}
}

func TestSavedResultRepairDoesNotReplayTool(t *testing.T) {
	x := contextTestExec(t)
	messages := []NativeMessage{{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: "saved-write", ToolName: "write_crm", Output: `{"receipt":"crm-42"}`}}}}
	x.Tools = nil
	if err := nativeEnsureSavedToolResults(x.Context, x, messages); err != nil {
		t.Fatal(err)
	}
	if err := nativeEnsureSavedToolResults(x.Context, x, messages); err != nil {
		t.Fatal(err)
	}
	calls, err := x.Store.ListToolCalls(x.Context, x.AppID, x.Run.ID)
	if err != nil || len(calls) != 1 {
		t.Fatalf("repair not idempotent: %d %v", len(calls), err)
	}
	raw, err := nativeSavedToolResult(x.Context, x, json.RawMessage(`{"tool_call_id":"saved-write"}`))
	if err != nil || !strings.Contains(raw, "crm-42") {
		t.Fatalf("lost receipt: %s %v", raw, err)
	}
}
