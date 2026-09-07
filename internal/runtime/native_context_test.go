package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const contextTestConfig = `{"native_context":{"enabled":true,"context_window":32000,"max_output_tokens":1000,"trigger_tokens":6000,"keep_recent_tokens":1000,"summary_tokens":500}}`

type contextTestModel struct {
	requests          []NativeModelRequest
	rounds, summaries int
	summaryErr        error
	incomplete        bool
	failAt            int
}

func (m *contextTestModel) Generate(_ context.Context, req NativeModelRequest) (*NativeModelResponse, error) {
	m.requests = append(m.requests, req)
	if m.failAt > 0 && len(m.requests) == m.failAt {
		return &NativeModelResponse{Usage: NativeUsage{InputTokens: 7}}, errors.New("provider unavailable")
	}
	msg := NativeMessage{Role: "assistant", Content: "done"}
	if len(m.requests) <= m.rounds {
		msg = NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: fmt.Sprint(len(m.requests)), ToolName: "read_probe", Input: json.RawMessage(`{}`)}}}
	}
	return &NativeModelResponse{Message: msg, Usage: NativeUsage{InputTokens: 100, OutputTokens: 10}}, nil
}
func (m *contextTestModel) Summarize(_ context.Context, _ string, _ int) (*NativeModelResponse, error) {
	m.summaries++
	return &NativeModelResponse{Message: NativeMessage{Role: "assistant", Content: "Inspected source files. Continue remaining checks; do not repeat completed actions."}, Usage: NativeUsage{InputTokens: 30, OutputTokens: 5}, Incomplete: m.incomplete}, m.summaryErr
}
func contextTestExec(t *testing.T) *ExecutionContext {
	t.Helper()
	registry := tools.NewRegistry()
	registry.Register(tools.Definition{Name: "read_probe", Description: "Read a bounded source page", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"content": strings.Repeat("source line\n", 230)})
	})
	return &ExecutionContext{Context: context.Background(), AppID: "a", Store: store.NewMemory(), Run: &agentcore.AgentRun{AppID: "a", ID: "r", RuntimeKind: agentcore.RuntimeNativeSDK, Input: agentcore.RunInput{Instructions: "Write a precise report; preserve document doc-42."}}, Agent: &agentcore.Agent{Name: "Native", RuntimeKind: agentcore.RuntimeNativeSDK, ExecutionConfig: json.RawMessage(contextTestConfig), AllowedTools: []string{"read_probe"}}, Tools: registry, AllowedTools: map[string]bool{"read_probe": true}}
}
func assertCompleteToolGroups(t *testing.T, messages []NativeMessage) {
	t.Helper()
	pending := map[string]bool{}
	for _, msg := range messages {
		if msg.Role != "tool" && len(pending) > 0 {
			t.Fatalf("unpaired calls before %s: %v", msg.Role, pending)
		}
		for _, block := range msg.Blocks {
			switch block.Type {
			case nativeBlockTypeToolCall:
				pending[block.ToolCallID] = true
			case nativeBlockTypeToolResult:
				if !pending[block.ToolCallID] {
					t.Fatalf("orphan result %s", block.ToolCallID)
				}
				delete(pending, block.ToolCallID)
			}
		}
	}
	if len(pending) > 0 {
		t.Fatalf("unresolved calls: %v", pending)
	}
}

func TestNativeContextLongRunBoundedAndDurable(t *testing.T) {
	x := contextTestExec(t)
	m := &contextTestModel{rounds: 120}
	result, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}, MaxToolSteps: 125})
	if err != nil {
		t.Fatal(err)
	}
	if m.summaries < 5 {
		t.Fatalf("expected repeated compaction, got %d", m.summaries)
	}
	for _, req := range m.requests {
		if n := nativeRequestTokens(req.SystemPrompt, req.Messages, req.Tools); n >= 6000 {
			t.Fatalf("unbounded request: %d", n)
		}
		assertCompleteToolGroups(t, req.Messages)
		found := false
		for _, msg := range req.Messages {
			if msg.Content == x.Run.Input.Instructions {
				found = true
			}
		}
		if !found {
			t.Fatal("lost user request")
		}
	}
	wantInput := int64(len(m.requests)*100 + m.summaries*30)
	if result.Usage.InputTokens != wantInput || result.Usage.OutputTokens != int64(len(m.requests)*10+m.summaries*5) {
		t.Fatalf("lost summary usage: %+v", result.Usage)
	}
	// Restarting a completed execution neither buys another response nor replays tools.
	restarted := &contextTestModel{}
	cached, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: restarted}})
	if err != nil || len(restarted.requests) != 0 || cached.Usage != result.Usage {
		t.Fatalf("restart: %v %+v", err, cached)
	}
	// A new user message with compaction disabled must use the bounded checkpoint,
	// not resurrect the archived prefix from legacy run output.
	x.Agent.ExecutionConfig = nil
	x.Run.Input.Metadata = map[string]any{"last_resume": map[string]any{"content": "Now verify the report.", "intent": "user_message"}}
	_, err = executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: restarted}})
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.requests) != 1 || len(restarted.requests[0].Messages) > len(result.Messages)+1 {
		t.Fatal("resume resurrected history")
	}
}

func TestNativeContextFailurePreservesHistoryAndUsage(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		x := contextTestExec(t)
		r, err := openNativeRecorder(x.Context, x, true)
		if err != nil {
			t.Fatal(err)
		}
		r.initialMessages(true)
		messages := []NativeMessage{{Role: "user", Content: strings.Repeat("history ", 3000)}, {Role: "assistant", Content: "recent"}}
		result := &nativeExecutionResult{Messages: messages}
		m := &contextTestModel{incomplete: incomplete}
		if !incomplete {
			m.summaryErr = errors.New("summary failed")
		}
		p, _ := nativeContextPolicy(x)
		compacted, err := nativeCompact(x.Context, r, m, p, "", nil, result, true)
		if err == nil || compacted || !reflect.DeepEqual(result.Messages, messages) {
			t.Fatal("failed summary replaced history")
		}
		if result.Usage.InputTokens != 30 {
			t.Fatal("failed summary usage lost")
		}
		state, _ := openNativeRecorder(x.Context, x, true)
		if state.state.Usage.InputTokens != 30 {
			t.Fatal("usage not durable")
		}
	}
}

func TestNativeContextFailedProviderRetainsUsage(t *testing.T) {
	x := contextTestExec(t)
	m := &contextTestModel{rounds: 3, failAt: 3}
	result, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}, MaxToolSteps: 10})
	if err == nil || result.Usage.InputTokens != 207 {
		t.Fatalf("partial usage: %+v %v", result, err)
	}
	usage, err := NativeCheckpointUsage(x.Context, x.Store, x.AppID, x.Run.ID)
	if err != nil || nativeUsageFromSummary(usage).InputTokens != 207 {
		t.Fatalf("checkpoint: %s %v", usage, err)
	}
}

func TestNativeContextInterruptedToolsNeverReplay(t *testing.T) {
	x := contextTestExec(t)
	r, _ := openNativeRecorder(x.Context, x, true)
	messages, _, _ := r.initialMessages(true)
	if err := r.save(x.Context, "tools", &nativeExecutionResult{Messages: messages}); err != nil {
		t.Fatal(err)
	}
	m := &contextTestModel{}
	_, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}})
	if err == nil || !strings.Contains(err.Error(), "interrupted during tool") || len(m.requests) > 0 {
		t.Fatalf("unsafe replay: %v", err)
	}
}

func TestNativeContextBudgetAndCancellationBeforeRequest(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		x := contextTestExec(t)
		if cancelled {
			ctx, cancel := context.WithCancel(x.Context)
			cancel()
			x.Context = ctx
		} else {
			x.Agent.ExecutionConfig = json.RawMessage(`{"native_context":{"max_total_tokens":1}}`)
		}
		m := &contextTestModel{}
		_, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}})
		if err == nil || len(m.requests) > 0 {
			t.Fatalf("request proceeded past guard: %v", err)
		}
	}
}

func TestNativeContextPolicyRejectsUnsafeLimits(t *testing.T) {
	x := contextTestExec(t)
	for _, config := range []string{`{"enabled":true}`, `{"enabled":true,"context_window":4096,"input_limit":4096}`, `{"enabled":true,"context_window":32000,"trigger_tokens":500,"keep_recent_tokens":500}`, `{"max_total_tokens":-1}`} {
		x.Agent.ExecutionConfig = json.RawMessage(`{"native_context":` + config + `}`)
		if _, err := nativeContextPolicy(x); err == nil {
			t.Fatalf("accepted %s", config)
		}
	}
	x.Agent.ExecutionConfig = nil
	p, err := nativeContextPolicy(x)
	if err != nil || p.Enabled {
		t.Fatal("must be opt-in")
	}
}

func TestNativeContextApprovalResumeExecutesExactlyOnce(t *testing.T) {
	x := contextTestExec(t)
	var count int32
	registerMutatingCounter(x.Tools, "create_document", &count)
	x.Agent.AllowedTools = []string{"create_document"}
	x.AllowedTools = map[string]bool{"create_document": true}
	x.Agent.ApprovalMode = agentcore.ApprovalModeAlways
	if err := x.Store.CreateRun(x.Context, x.Run); err != nil {
		t.Fatal(err)
	}
	m := &contextApprovalModel{fakeNativeModel: fakeNativeModel{responses: []NativeModelResponse{{Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "approved-call", ToolName: "create_document", Input: json.RawMessage(`{}`)}}}}}}}
	first, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}})
	if err != nil || !first.AwaitingApproval || atomic.LoadInt32(&count) != 0 {
		t.Fatalf("approval: %+v %v", first, err)
	}
	interactions, _ := x.Store.ListInteractions(x.Context, x.AppID, x.Run.ID)
	if len(interactions) != 1 {
		t.Fatalf("interactions: %v", interactions)
	}
	resolveInteraction(t, x.Store, x.AppID, x.Run.ID, interactions[0].ID, "approve", "")
	x.Run.Input.Metadata = map[string]any{"last_resume": map[string]any{"intent": "approve"}}
	secondModel := &contextApprovalModel{}
	_, err = executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: secondModel}})
	if err != nil || atomic.LoadInt32(&count) != 1 {
		t.Fatalf("approved execution: %d %v", count, err)
	}
	if len(secondModel.requests) != 1 {
		t.Fatal("no continuation")
	}
	assertCompleteToolGroups(t, secondModel.requests[0].Messages)
	_, err = executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: secondModel}})
	if err != nil || atomic.LoadInt32(&count) != 1 || len(secondModel.requests) != 1 {
		t.Fatalf("duplicate approval execution: %v", err)
	}
}

type contextApprovalModel struct{ fakeNativeModel }

func (*contextApprovalModel) Summarize(context.Context, string, int) (*NativeModelResponse, error) {
	return nil, errors.New("unexpected compaction in short approval test")
}

type summaryOptionsModel struct {
	fakeEinoToolCallingModel
	options *einomodel.Options
}

func (m *summaryOptionsModel) Generate(_ context.Context, _ []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	m.options = einomodel.GetCommonOptions(nil, opts...)
	return &schema.Message{Role: schema.Assistant, Content: "summary", ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"}}, nil
}
func TestNativeSummaryDisablesToolsOnlyForSummaryRequest(t *testing.T) {
	m := &summaryOptionsModel{fakeEinoToolCallingModel: fakeEinoToolCallingModel{boundTools: []*schema.ToolInfo{{Name: "write"}}}}
	adapter := einoNativeModel{model: m}
	resp, err := adapter.Summarize(context.Background(), "historical input", 500)
	if err != nil || resp.Incomplete {
		t.Fatalf("summary: %v %+v", err, resp)
	}
	if m.options.Tools == nil || len(m.options.Tools) > 0 || m.options.ToolChoice == nil || *m.options.ToolChoice != schema.ToolChoiceForbidden || *m.options.MaxTokens != 500 {
		t.Fatalf("unsafe summary options: %+v", m.options)
	}
	if len(m.boundTools) != 1 {
		t.Fatal("summary mutated normal tool bindings")
	}
}

func TestNativeReadFilesPreservesJSONCursorEnvelope(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"files": []any{map[string]any{"content": strings.Repeat("<", 8192), "next_start_line": 42, "next_start_char": 10}}})
	visible := prepareNativeToolResultForModel("read_files", string(body), false)
	if visible.Compacted || visible.Content != string(body) || !json.Valid([]byte(visible.Content)) {
		t.Fatal("structured read cursor clipped")
	}
}

func TestNativeContextCompactionKeepsSkillAndWholeParallelGroup(t *testing.T) {
	x := contextTestExec(t)
	r, _ := openNativeRecorder(x.Context, x, true)
	r.initialMessages(true)
	messages := []NativeMessage{
		{Role: "user", Content: x.Run.Input.Instructions},
		{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolName: "read_skill", ToolCallID: "skill", Input: json.RawMessage(`{}`)}, {Type: nativeBlockTypeToolCall, ToolName: "read_probe", ToolCallID: "parallel", Input: json.RawMessage(`{}`)}}},
		{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolName: "read_skill", ToolCallID: "skill", Output: "Mandatory skill instructions"}}},
		{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolName: "read_probe", ToolCallID: "parallel", Output: "related source"}}},
		{Role: "assistant", Content: strings.Repeat("old inspected facts ", 1500)},
		{Role: "assistant", Content: "recent work"},
	}
	result := &nativeExecutionResult{Messages: messages}
	p, _ := nativeContextPolicy(x)
	ok, err := nativeCompact(x.Context, r, &contextTestModel{}, p, "", nil, result, true)
	if err != nil || !ok {
		t.Fatalf("compaction: %v", err)
	}
	assertCompleteToolGroups(t, result.Messages)
	found := false
	for _, msg := range result.Messages {
		for _, b := range msg.Blocks {
			if b.ToolCallID == "skill" && b.Output == "Mandatory skill instructions" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("skill instructions lost")
	}
}

func TestNativeContextStaleCompactorCannotReplaceCheckpoint(t *testing.T) {
	x := contextTestExec(t)
	r, _ := openNativeRecorder(x.Context, x, true)
	messages, _, _ := r.initialMessages(true)
	result := &nativeExecutionResult{Messages: messages}
	if err := r.save(x.Context, "ready", result); err != nil {
		t.Fatal(err)
	}
	stale, _ := openNativeRecorder(x.Context, x, true)
	if err := r.save(x.Context, "ready", result); err != nil {
		t.Fatal(err)
	}
	err := stale.save(x.Context, "ready", result)
	if !errors.Is(err, errNativeCheckpoint) || !errors.Is(err, agentcore.ErrNativeStateConflict) {
		t.Fatalf("CAS not enforced: %v", err)
	}
}

type overflowingContextModel struct{ contextTestModel }

func (m *overflowingContextModel) Generate(_ context.Context, req NativeModelRequest) (*NativeModelResponse, error) {
	m.requests = append(m.requests, req)
	return nil, errors.New("context_length_exceeded")
}
func TestNativeContextOverflowRecoveryIsBounded(t *testing.T) {
	x := contextTestExec(t)
	r, _ := openNativeRecorder(x.Context, x, true)
	r.initialMessages(true)
	messages := []NativeMessage{{Role: "user", Content: x.Run.Input.Instructions}, {Role: "assistant", Content: strings.Repeat("old fact ", 600)}, {Role: "assistant", Content: "recent work"}}
	if err := r.save(x.Context, "ready", &nativeExecutionResult{Messages: messages}); err != nil {
		t.Fatal(err)
	}
	m := &overflowingContextModel{}
	_, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}, MaxToolSteps: 100})
	if err == nil || len(m.requests) != 2 || m.summaries != 1 {
		t.Fatalf("unbounded retry: requests=%d summaries=%d err=%v", len(m.requests), m.summaries, err)
	}
}

type interruptedUsageStream struct{ sent bool }

func (s *interruptedUsageStream) Recv() (*NativeModelResponse, error) {
	if s.sent {
		return nil, errors.New("stream interrupted")
	}
	s.sent = true
	return &NativeModelResponse{Message: NativeMessage{Role: "assistant", Content: "partial"}, Usage: NativeUsage{InputTokens: 70, OutputTokens: 9}}, nil
}
func (*interruptedUsageStream) Close() {}
func TestNativeContextInterruptedStreamUsagePersistsAfterCancellation(t *testing.T) {
	x := contextTestExec(t)
	response, _, err := collectNativeModelStream(x.Context, x, &interruptedUsageStream{})
	if err == nil || response == nil || response.Usage.InputTokens != 70 {
		t.Fatalf("stream usage lost: %+v %v", response, err)
	}
	r, _ := openNativeRecorder(x.Context, x, true)
	messages, _, _ := r.initialMessages(true)
	ctx, cancel := context.WithCancel(x.Context)
	cancel()
	if err := r.checkpointUsage(ctx, &nativeExecutionResult{Messages: messages}, response, "failed_response"); err != nil {
		t.Fatal(err)
	}
	loaded, _ := openNativeRecorder(x.Context, x, true)
	if loaded.state.Usage.InputTokens != 70 || loaded.state.Usage.OutputTokens != 9 {
		t.Fatal("cancelled response accounting lost")
	}
}
