package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	openaischema "github.com/cloudwego/eino/schema/openai"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestNativeAdapterExecutesModelToolRounds(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	registry.Register(tools.Definition{
		Name:        "read_one",
		Description: "read one",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		},
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		time.Sleep(150 * time.Millisecond)
		return json.RawMessage(`{"content":"one"}`), nil
	})
	registry.Register(tools.Definition{
		Name:        "read_two",
		Description: "read two",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		},
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		time.Sleep(150 * time.Millisecond)
		return json.RawMessage(`{"content":"two"}`), nil
	})
	run := &agentcore.AgentRun{
		ID:          "run-native",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Input:       agentcore.RunInput{Instructions: "inspect"},
		RuntimeKind: agentcore.RuntimeNativeSDK,
	}
	eventSink := &testEventSink{}
	adapter := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: &fakeNativeModel{
			responses: []NativeModelResponse{
				{
					Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
						{Type: nativeBlockTypeText, Text: "Reading files."},
						{Type: nativeBlockTypeToolCall, ToolCallID: "call-1", ToolName: "read_one", Input: json.RawMessage(`{"path":"one.md"}`)},
						{Type: nativeBlockTypeToolCall, ToolCallID: "call-2", ToolName: "read_two", Input: json.RawMessage(`{"path":"two.md"}`)},
					}},
					Usage: NativeUsage{InputTokens: 3, OutputTokens: 4},
				},
				{
					Message: NativeMessage{Role: "assistant", Content: "done"},
					Usage:   NativeUsage{InputTokens: 5, CachedInputTokens: 1, OutputTokens: 6, ReasoningOutputTokens: 2},
				},
			},
		}},
		MaxToolSteps: 4,
	})

	start := time.Now()
	result, err := adapter.Execute(&ExecutionContext{
		Context:      context.Background(),
		AppID:        "app-a",
		Store:        mem,
		Agent:        &agentcore.Agent{Name: "Native", RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: []string{"read_one", "read_two"}},
		Run:          run,
		Tools:        registry,
		AllowedTools: map[string]bool{"read_one": true, "read_two": true},
		EventSink:    eventSink,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("execute native: %v", err)
	}
	if result.AssistantMessage != "done" {
		t.Fatalf("unexpected assistant message: %q", result.AssistantMessage)
	}
	if elapsed >= 280*time.Millisecond {
		t.Fatalf("expected read-only tools to run in parallel, took %v", elapsed)
	}
	if !strings.Contains(string(result.OutputSummary), `"input_tokens":8`) || !strings.Contains(string(result.OutputSummary), `"reasoning_output_tokens":2`) {
		t.Fatalf("expected aggregated usage in output summary, got %s", string(result.OutputSummary))
	}
	if !eventSink.hasType("assistant_message_started") || !eventSink.hasType("tool_call_started") || !eventSink.hasType("tool_call_finished") {
		t.Fatalf("expected assistant and tool events, got %#v", eventSink.events)
	}
	calls, err := mem.ListToolCalls(context.Background(), "app-a", "run-native")
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected two durable tool calls, got %#v", calls)
	}
	if calls[0].ToolName != "read_one" || calls[0].Mutating || calls[0].Error != "" {
		t.Fatalf("unexpected first tool call: %#v", calls[0])
	}
	if !strings.Contains(string(calls[0].Output), `"runtime_kind":"native_sdk"`) {
		t.Fatalf("expected native output metadata, got %s", string(calls[0].Output))
	}
	messages, err := mem.ListMessages(context.Background(), "app-a", "run-native")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Role != "assistant" || messages[0].MessageType != "assistant_turn" {
		t.Fatalf("expected one assistant_turn message, got %#v", messages)
	}
	if messages[0].Content != "done" {
		t.Fatalf("expected final assistant content, got %#v", messages[0])
	}
	if len(messages[0].ContentBlocks) == 0 || len(messages[0].ToolInvocations) == 0 {
		t.Fatalf("expected content blocks and tool invocations, got %#v", messages[0])
	}
	invocations := nativeStoredToolInvocations(t, messages[0])
	if len(invocations) != 2 || invocations[0].ToolName != "read_one" || invocations[1].ToolName != "read_two" {
		t.Fatalf("unexpected persisted tool invocations: %#v", invocations)
	}
}

func TestNativeAdapterFallsBackWithoutModelFactory(t *testing.T) {
	result, err := NewNativeAdapter().Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent:   &agentcore.Agent{Name: "Native"},
		Run: &agentcore.AgentRun{
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "ticket", ID: "T-1"},
			Input:       agentcore.RunInput{Instructions: "summarize"},
			RuntimeKind: agentcore.RuntimeNativeSDK,
		},
	})
	if err != nil {
		t.Fatalf("execute fallback: %v", err)
	}
	if !strings.Contains(result.AssistantMessage, "completed a native_sdk run") {
		t.Fatalf("unexpected fallback message: %q", result.AssistantMessage)
	}
}

func TestNativeAdapterIncludesSkillInstructionsInSystemPrompt(t *testing.T) {
	model := &fakeNativeModel{
		responses: []NativeModelResponse{{Message: NativeMessage{Role: "assistant", Content: "done"}}},
	}
	_, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: model},
	}).Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent: &agentcore.Agent{
			Name:         "Native",
			RuntimeKind:  agentcore.RuntimeNativeSDK,
			SystemPrompt: "Base prompt.",
		},
		Run: &agentcore.AgentRun{
			ID:          "run-skill-prompt",
			AppID:       "app-a",
			RuntimeKind: agentcore.RuntimeNativeSDK,
			Target:      agentcore.TargetRef{Type: "task", ID: "T-1"},
			Input:       agentcore.RunInput{Instructions: "summarize"},
		},
		Store:             store.NewMemory(),
		Tools:             tools.NewRegistry(),
		AllowedTools:      map[string]bool{},
		SkillInstructions: "Always ask for approval.",
		SkillPolicy: skills.Policy{
			CompletionRequiresInteractionKinds: []string{skills.InteractionKindApprovalRequest},
		},
	})
	if err != nil {
		t.Fatalf("execute native: %v", err)
	}
	if len(model.requests) != 1 || !strings.Contains(model.requests[0].SystemPrompt, "Skill instructions:\nAlways ask for approval.") {
		t.Fatalf("expected skill instructions in system prompt, got %#v", model.requests)
	}
}

func TestNativeAdapterRequestUserInputPausesAndPersistsInteraction(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	registry.Register(tools.Definition{
		Name:        "after_pause",
		Description: "must not run after pause",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Mutating: true,
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		t.Fatal("tool after interaction pause should not execute")
		return json.RawMessage(`{}`), nil
	})
	model := &fakeNativeModel{
		responses: []NativeModelResponse{{
			Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
				{Type: nativeBlockTypeText, Text: "Need input."},
				{Type: nativeBlockTypeToolCall, ToolCallID: "input-1", ToolName: "request_user_input", Input: json.RawMessage(`{
					"questions": [{
						"id": "owner",
						"header": "Owner",
						"question": "Who owns this?",
						"isOther": true,
						"options": [{"label":"Sales"}, {"label":"Support"}]
					}]
				}`)},
				{Type: nativeBlockTypeToolCall, ToolCallID: "after-1", ToolName: "after_pause", Input: json.RawMessage(`{}`)},
			}},
		}},
	}
	result, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: model},
		MaxToolSteps: 2,
	}).Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent: &agentcore.Agent{
			Name:         "Native",
			RuntimeKind:  agentcore.RuntimeNativeSDK,
			AllowedTools: []string{"request_user_input", "after_pause"},
		},
		Run: &agentcore.AgentRun{
			ID:          "run-input",
			AppID:       "app-a",
			RuntimeKind: agentcore.RuntimeNativeSDK,
			Target:      agentcore.TargetRef{Type: "task", ID: "T-1"},
			Input:       agentcore.RunInput{Instructions: "ask"},
		},
		Tools:        registry,
		AllowedTools: map[string]bool{"request_user_input": true, "after_pause": true},
	})
	if err != nil {
		t.Fatalf("execute native: %v", err)
	}
	if !result.AwaitingInput || result.WaitForApproval {
		t.Fatalf("expected awaiting input pause, got %#v", result)
	}
	if len(model.requests) != 1 {
		t.Fatalf("expected model loop to stop after interaction pause, got %d requests", len(model.requests))
	}
	if !nativeRequestHasTool(model.requests[0], "request_user_input") {
		t.Fatalf("expected request_user_input tool definition, got %#v", model.requests[0].Tools)
	}
	interactions, err := mem.ListInteractions(context.Background(), "app-a", "run-input")
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 {
		t.Fatalf("expected one interaction, got %#v", interactions)
	}
	interaction := interactions[0]
	if interaction.InteractionKind != nativeInteractionKindRequestUserInput || interaction.Status != "pending" {
		t.Fatalf("unexpected interaction: %#v", interaction)
	}
	if interaction.Title != "Input requested" || interaction.Summary != "Owner: Who owns this?" {
		t.Fatalf("unexpected interaction text: %#v", interaction)
	}
	if !strings.Contains(string(interaction.RequestPayload), `"request_schema":"request_user_input_v1"`) {
		t.Fatalf("expected request schema in payload, got %s", string(interaction.RequestPayload))
	}
	calls, err := mem.ListToolCalls(context.Background(), "app-a", "run-input")
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(calls) != 1 || calls[0].ToolName != "request_user_input" || !strings.Contains(nativeToolCallAuditOutput(t, calls[0]), `"pause_reason":"human_input"`) {
		t.Fatalf("unexpected durable tool call: %#v", calls)
	}
	messages, err := mem.ListMessages(context.Background(), "app-a", "run-input")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected assistant turn and trailing tool result, got %#v", messages)
	}
	if messages[0].Role != "assistant" || messages[0].MessageType != "assistant_turn" || len(messages[0].ToolInvocations) == 0 {
		t.Fatalf("unexpected assistant persisted message: %#v", messages[0])
	}
	if messages[1].Role != "tool" || messages[1].MessageType != "tool_result" || !strings.Contains(messages[1].Content, agentcore.PauseReasonHumanInput) {
		t.Fatalf("unexpected tool result persisted message: %#v", messages[1])
	}
}

func TestNativeAdapterUpdatePlanPersistsArtifactAndEmitsEvent(t *testing.T) {
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-plan",
		AppID:       "app-a",
		RuntimeKind: agentcore.RuntimeNativeSDK,
		Target:      agentcore.TargetRef{Type: "message_generation_task", ID: "task-1"},
		Input:       agentcore.RunInput{Instructions: "analyze"},
	}
	eventSink := &testEventSink{}
	model := &fakeNativeModel{
		responses: []NativeModelResponse{
			{
				Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
					{Type: nativeBlockTypeText, Text: "Planning the analysis."},
					{Type: nativeBlockTypeToolCall, ToolCallID: "plan-1", ToolName: "update_plan", Input: json.RawMessage(`{
						"explanation": "This requires multiple data sources.",
						"plan": [
							{"step": "Inspect available analytics models", "status": "in_progress"},
							{"step": "Fetch conversion and traffic data", "status": "pending"},
							{"step": "Synthesize drivers and risks", "status": "pending"}
						],
						"metadata": {
							"intent": "diagnostic_analysis",
							"data_sources": ["analytics events", "conversion goals"],
							"expected_outputs": ["chart", "summary"]
						}
					}`)},
				}},
			},
			{
				Message: NativeMessage{Role: "assistant", Content: "Traffic quality analysis complete."},
			},
		},
	}

	result, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: model},
		MaxToolSteps: 2,
	}).Execute(&ExecutionContext{
		Context:        context.Background(),
		AppID:          "app-a",
		Store:          mem,
		Agent:          &agentcore.Agent{Name: "Native", RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: []string{"update_plan"}},
		Run:            run,
		Tools:          tools.NewRegistry(),
		AllowedTools:   map[string]bool{"update_plan": true},
		ArtifactWriter: testArtifactWriter{store: mem, run: run},
		EventSink:      eventSink,
	})
	if err != nil {
		t.Fatalf("execute native: %v", err)
	}
	if result.AssistantMessage != "Traffic quality analysis complete." {
		t.Fatalf("unexpected assistant message: %q", result.AssistantMessage)
	}
	if len(model.requests) == 0 || !nativeRequestHasTool(model.requests[0], "update_plan") {
		t.Fatalf("expected update_plan tool definition, got %#v", model.requests)
	}
	if !eventSink.hasType("plan_updated") {
		t.Fatalf("expected plan_updated event, got %#v", eventSink.events)
	}
	artifacts, err := mem.ListArtifacts(context.Background(), "app-a", "run-plan")
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].ArtifactType != "run_plan" || artifacts[0].Format != "json" {
		t.Fatalf("unexpected artifacts: %#v", artifacts)
	}
	if !strings.Contains(artifacts[0].InlineContent, `"intent":"diagnostic_analysis"`) || !strings.Contains(artifacts[0].InlineContent, `"status":"in_progress"`) {
		t.Fatalf("unexpected plan artifact content: %s", artifacts[0].InlineContent)
	}
	calls, err := mem.ListToolCalls(context.Background(), "app-a", "run-plan")
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(calls) != 1 || calls[0].ToolName != "update_plan" || calls[0].Mutating {
		t.Fatalf("unexpected tool call: %#v", calls)
	}
}

func TestNativeAdapterRequestApprovalPausesAndPersistsInteraction(t *testing.T) {
	mem := store.NewMemory()
	result, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: &fakeNativeModel{
			responses: []NativeModelResponse{{
				Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
					{Type: nativeBlockTypeToolCall, ToolCallID: "approval-1", ToolName: "request_approval", Input: json.RawMessage(`{
						"phase": " PRD ",
						"preview_panel_key": " PRD_DRAFT ",
						"title": " Approve PRD ",
						"summary": " Review the draft. "
					}`)},
				}},
			}},
		}},
	}).Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent: &agentcore.Agent{
			Name:         "Native",
			RuntimeKind:  agentcore.RuntimeNativeSDK,
			AllowedTools: []string{"request_approval"},
		},
		Run: &agentcore.AgentRun{
			ID:          "run-approval",
			AppID:       "app-a",
			RuntimeKind: agentcore.RuntimeNativeSDK,
			Target:      agentcore.TargetRef{Type: "epic", ID: "E-1"},
			Input:       agentcore.RunInput{Instructions: "plan"},
		},
		Tools:        tools.NewRegistry(),
		AllowedTools: map[string]bool{"request_approval": true},
	})
	if err != nil {
		t.Fatalf("execute native: %v", err)
	}
	if !result.WaitForApproval || result.AwaitingInput {
		t.Fatalf("expected approval pause, got %#v", result)
	}
	interactions, err := mem.ListInteractions(context.Background(), "app-a", "run-approval")
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 {
		t.Fatalf("expected one interaction, got %#v", interactions)
	}
	if interactions[0].InteractionKind != nativeInteractionKindApprovalRequest || interactions[0].Title != "Approve PRD" || interactions[0].Summary != "Review the draft." {
		t.Fatalf("unexpected approval interaction: %#v", interactions[0])
	}
	payload := string(interactions[0].RequestPayload)
	for _, snippet := range []string{`"phase":"prd"`, `"preview_panel_key":"prd_draft"`, `"request_schema":"approval_request_v1"`} {
		if !strings.Contains(payload, snippet) {
			t.Fatalf("expected payload to contain %s, got %s", snippet, payload)
		}
	}
}

func TestNativeAdapterRequiresApprovalBeforeMutatingTool(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	executed := false
	registry.Register(tools.Definition{
		Name:        "write_file",
		Description: "write file",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		},
		Mutating: true,
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		executed = true
		return json.RawMessage(`{"ok":true}`), nil
	})
	result, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: &fakeNativeModel{
			responses: []NativeModelResponse{{
				Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
					{Type: nativeBlockTypeToolCall, ToolCallID: "write-1", ToolName: "write_file", Input: json.RawMessage(`{"path":"README.md"}`)},
				}},
			}},
		}},
	}).Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent: &agentcore.Agent{
			Name:         "Native",
			RuntimeKind:  agentcore.RuntimeNativeSDK,
			AllowedTools: []string{"write_file"},
			ApprovalMode: agentcore.ApprovalModeAlways,
		},
		Run: &agentcore.AgentRun{
			ID:          "run-mutating-approval",
			AppID:       "app-a",
			RuntimeKind: agentcore.RuntimeNativeSDK,
			Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
			Input:       agentcore.RunInput{Instructions: "write"},
		},
		Tools:        registry,
		AllowedTools: map[string]bool{"write_file": true},
	})
	if err != nil {
		t.Fatalf("execute native: %v", err)
	}
	if executed {
		t.Fatal("mutating tool executed before approval")
	}
	if !result.WaitForApproval || result.AwaitingInput {
		t.Fatalf("expected approval pause, got %#v", result)
	}
	interactions, err := mem.ListInteractions(context.Background(), "app-a", "run-mutating-approval")
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != nativeInteractionKindApprovalRequest || interactions[0].Title != "Approve tool call" {
		t.Fatalf("unexpected approval interaction: %#v", interactions)
	}
	if !strings.Contains(string(interactions[0].RequestPayload), `"tool_name":"write_file"`) || !strings.Contains(string(interactions[0].RequestPayload), `"mutating":true`) {
		t.Fatalf("expected tool approval payload, got %s", string(interactions[0].RequestPayload))
	}
	calls, err := mem.ListToolCalls(context.Background(), "app-a", "run-mutating-approval")
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(calls) != 1 || calls[0].ToolName != "write_file" || !calls[0].Mutating || !calls[0].ApprovalRequired || calls[0].Error != "" {
		t.Fatalf("unexpected approval tool call audit: %#v", calls)
	}
	if !strings.Contains(nativeToolCallAuditOutput(t, calls[0]), `"approval_required":true`) || !strings.Contains(nativeToolCallAuditOutput(t, calls[0]), `"pause_reason":"human_approval"`) {
		t.Fatalf("expected approval output audit, got %s", nativeToolCallAuditOutput(t, calls[0]))
	}
}

func TestNativeAdapterExecutesMutatingToolWhenApprovalNever(t *testing.T) {
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	executed := false
	registry.Register(tools.Definition{
		Name:        "write_file",
		Description: "write file",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
		},
		Mutating: true,
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		executed = true
		return json.RawMessage(`{"ok":true}`), nil
	})
	result, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: &fakeNativeModel{
			responses: []NativeModelResponse{
				{
					Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
						{Type: nativeBlockTypeToolCall, ToolCallID: "write-1", ToolName: "write_file", Input: json.RawMessage(`{"path":"README.md"}`)},
					}},
				},
				{Message: NativeMessage{Role: "assistant", Content: "done"}},
			},
		}},
	}).Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent: &agentcore.Agent{
			Name:         "Native",
			RuntimeKind:  agentcore.RuntimeNativeSDK,
			AllowedTools: []string{"write_file"},
			ApprovalMode: agentcore.ApprovalModeNever,
		},
		Run: &agentcore.AgentRun{
			ID:          "run-mutating-no-approval",
			AppID:       "app-a",
			RuntimeKind: agentcore.RuntimeNativeSDK,
			Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
			Input:       agentcore.RunInput{Instructions: "write"},
		},
		Tools:        registry,
		AllowedTools: map[string]bool{"write_file": true},
	})
	if err != nil {
		t.Fatalf("execute native: %v", err)
	}
	if !executed {
		t.Fatal("expected mutating tool to execute when approval mode is never")
	}
	if result.WaitForApproval || result.AwaitingInput || result.AssistantMessage != "done" {
		t.Fatalf("unexpected result: %#v", result)
	}
	calls, err := mem.ListToolCalls(context.Background(), "app-a", "run-mutating-no-approval")
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(calls) != 1 || calls[0].ToolName != "write_file" || !calls[0].Mutating || calls[0].ApprovalRequired {
		t.Fatalf("unexpected mutating call audit: %#v", calls)
	}
}

func TestNativeAdapterAcceptsLegacyHumanInputAlias(t *testing.T) {
	mem := store.NewMemory()
	result, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: &fakeNativeModel{
			responses: []NativeModelResponse{{
				Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
					{Type: nativeBlockTypeToolCall, ToolCallID: "legacy-1", ToolName: "request_human_input", Input: json.RawMessage(`{
						"questions": [{
							"id": "q1",
							"text": "Pick one",
							"options": [
								{"value":"sales","label":"Sales"},
								{"value":"other","label":"Other","freetext":true}
							]
						}]
					}`)},
				}},
			}},
		}},
	}).Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent: &agentcore.Agent{
			Name:         "Native",
			RuntimeKind:  agentcore.RuntimeNativeSDK,
			AllowedTools: []string{"request_user_input"},
		},
		Run: &agentcore.AgentRun{
			ID:          "run-legacy-input",
			AppID:       "app-a",
			RuntimeKind: agentcore.RuntimeNativeSDK,
			Target:      agentcore.TargetRef{Type: "task", ID: "T-1"},
			Input:       agentcore.RunInput{Instructions: "ask"},
		},
		Tools:        tools.NewRegistry(),
		AllowedTools: map[string]bool{"request_user_input": true},
	})
	if err != nil {
		t.Fatalf("execute native: %v", err)
	}
	if !result.AwaitingInput {
		t.Fatalf("expected awaiting input pause, got %#v", result)
	}
	calls, err := mem.ListToolCalls(context.Background(), "app-a", "run-legacy-input")
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(calls) != 1 || calls[0].ToolName != "request_user_input" || !strings.Contains(nativeToolCallAuditOutput(t, calls[0]), `"question":"Pick one"`) {
		t.Fatalf("unexpected legacy tool call output: %#v", calls)
	}
}

func TestNativeAdapterReplaysPausedTranscriptOnResume(t *testing.T) {
	mem := store.NewMemory()
	firstModel := &fakeNativeModel{
		responses: []NativeModelResponse{{
			Message: NativeMessage{Role: "assistant", Blocks: []NativeBlock{
				{Type: nativeBlockTypeText, Text: "Need input."},
				{Type: nativeBlockTypeToolCall, ToolCallID: "input-1", ToolName: "request_user_input", Input: json.RawMessage(`{
					"questions": [{"id":"owner","question":"Who owns this?"}]
				}`)},
			}},
		}},
	}
	run := &agentcore.AgentRun{
		ID:          "run-resume-native",
		AppID:       "app-a",
		RuntimeKind: agentcore.RuntimeNativeSDK,
		Target:      agentcore.TargetRef{Type: "task", ID: "T-1"},
		Input:       agentcore.RunInput{Instructions: "ask"},
	}
	execCtx := &ExecutionContext{
		Context:      context.Background(),
		AppID:        "app-a",
		Store:        mem,
		Agent:        &agentcore.Agent{Name: "Native", RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: []string{"request_user_input"}},
		Run:          run,
		Tools:        tools.NewRegistry(),
		AllowedTools: map[string]bool{"request_user_input": true},
	}
	first, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: firstModel},
	}).Execute(execCtx)
	if err != nil {
		t.Fatalf("first execute native: %v", err)
	}
	if !first.AwaitingInput {
		t.Fatalf("expected first execution to pause for input, got %#v", first)
	}
	if !strings.Contains(string(first.OutputSummary), `"native_messages"`) {
		t.Fatalf("expected paused output summary to include transcript, got %s", string(first.OutputSummary))
	}

	resumeModel := &fakeNativeModel{
		responses: []NativeModelResponse{{
			Message: NativeMessage{Role: "assistant", Content: "Thanks, continuing with Sales."},
		}},
	}
	resumedRun := *run
	resumedRun.OutputSummary = first.OutputSummary
	resumedRun.Input.Metadata = map[string]any{
		"last_resume": map[string]any{
			"intent":            "reply",
			"content":           "Sales owns it.",
			"response_payload":  json.RawMessage(`{"answers":{"owner":{"answers":["Sales"]}}}`),
			"external_actor_id": "user-1",
		},
	}
	second, err := NewNativeAdapterWithConfig(NativeConfig{
		ModelFactory: fakeNativeFactory{model: resumeModel},
	}).Execute(&ExecutionContext{
		Context:      context.Background(),
		AppID:        "app-a",
		Store:        mem,
		Agent:        execCtx.Agent,
		Run:          &resumedRun,
		Tools:        tools.NewRegistry(),
		AllowedTools: map[string]bool{"request_user_input": true},
	})
	if err != nil {
		t.Fatalf("resume execute native: %v", err)
	}
	if second.AwaitingInput || second.WaitForApproval || second.AssistantMessage != "Thanks, continuing with Sales." {
		t.Fatalf("unexpected resumed result: %#v", second)
	}
	if len(resumeModel.requests) != 1 {
		t.Fatalf("expected one resumed model request, got %d", len(resumeModel.requests))
	}
	messages := resumeModel.requests[0].Messages
	if len(messages) < 4 {
		t.Fatalf("expected paused transcript plus resume message, got %#v", messages)
	}
	if messages[0].Content != "ask" {
		t.Fatalf("expected original prompt to be replayed, got %#v", messages[0])
	}
	if !nativeMessagesContainToolResult(messages, "input-1", agentcore.PauseReasonHumanInput) {
		t.Fatalf("expected paused tool result in replay, got %#v", messages)
	}
	last := messages[len(messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "Sales owns it.") || !strings.Contains(last.Content, `"answers"`) {
		t.Fatalf("expected structured human resume message, got %#v", last)
	}
}

func TestToEinoToolInfosConvertsNestedJSONSchema(t *testing.T) {
	infos, err := toEinoToolInfos([]tools.Definition{{
		Name:        "create_task",
		Description: "Create a task",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"title", "metadata"},
			"properties": map[string]any{
				"title": map[string]any{
					"type":        "string",
					"description": "Task title",
				},
				"priority": map[string]any{
					"type": "string",
					"enum": []any{"low", "high"},
				},
				"metadata": map[string]any{
					"type":     "object",
					"required": []any{"tags"},
					"properties": map[string]any{
						"tags": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
						"public": map[string]any{"type": "boolean"},
					},
				},
			},
		},
	}})
	if err != nil {
		t.Fatalf("convert tool infos: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "create_task" {
		t.Fatalf("unexpected tool infos: %#v", infos)
	}
	schema, err := infos[0].ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("convert params to json schema: %v", err)
	}
	raw, _ := json.Marshal(schema)
	schemaJSON := string(raw)
	for _, snippet := range []string{
		`"title"`,
		`"Task title"`,
		`"priority"`,
		`"high"`,
		`"metadata"`,
		`"tags"`,
	} {
		if !strings.Contains(schemaJSON, snippet) {
			t.Fatalf("expected converted schema to contain %s, got %s", snippet, schemaJSON)
		}
	}
}

func nativeRequestHasTool(req NativeModelRequest, name string) bool {
	for _, def := range req.Tools {
		if def.Name == name {
			return true
		}
	}
	return false
}

func nativeToolCallAuditOutput(t *testing.T, call agentcore.ToolCall) string {
	t.Helper()
	var audit struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(call.Output, &audit); err != nil {
		t.Fatalf("parse tool call audit output: %v", err)
	}
	return audit.Output
}

func nativeStoredToolInvocations(t *testing.T, message agentcore.AgentRunMessage) []nativeToolInvocation {
	t.Helper()
	var invocations []nativeToolInvocation
	if err := json.Unmarshal(message.ToolInvocations, &invocations); err != nil {
		t.Fatalf("parse stored tool invocations: %v", err)
	}
	return invocations
}

func nativeMessagesContainToolResult(messages []NativeMessage, toolCallID string, text string) bool {
	for _, message := range messages {
		for _, block := range message.Blocks {
			if block.Type == nativeBlockTypeToolResult && block.ToolCallID == toolCallID && strings.Contains(block.Output, text) {
				return true
			}
		}
	}
	return false
}

func TestEinoChatModelFactoryConvertsNativeRequests(t *testing.T) {
	model := &fakeEinoToolCallingModel{
		response: schema.AssistantMessage("need context", []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "get_context",
				Arguments: `{"scope":"target"}`,
			},
		}}),
	}
	model.response.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
		PromptTokens:     7,
		CompletionTokens: 11,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: 3,
		},
		CompletionTokensDetails: schema.CompletionTokensDetails{
			ReasoningTokens: 5,
		},
	}}
	factory := EinoChatModelFactory{Model: model}
	nativeModel, err := factory.ResolveNativeModel(context.Background(), &ExecutionContext{}, []tools.Definition{{
		Name:        "get_context",
		Description: "Get context",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"scope": map[string]any{"type": "string"}},
		},
	}})
	if err != nil {
		t.Fatalf("resolve native model: %v", err)
	}
	response, err := nativeModel.Generate(context.Background(), NativeModelRequest{
		SystemPrompt: "system",
		Messages:     []NativeMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(model.boundTools) != 1 || model.boundTools[0].Name != "get_context" {
		t.Fatalf("expected tool binding, got %#v", model.boundTools)
	}
	if len(model.lastInput) != 2 || model.lastInput[0].Role != schema.System || model.lastInput[1].Role != schema.User {
		t.Fatalf("unexpected model input: %#v", model.lastInput)
	}
	if response.Usage.InputTokens != 7 || response.Usage.CachedInputTokens != 3 || response.Usage.OutputTokens != 11 || response.Usage.ReasoningOutputTokens != 5 {
		t.Fatalf("usage was not converted: %#v", response.Usage)
	}
	toolCalls := nativeToolCallBlocks(response.Message)
	if len(toolCalls) != 1 || toolCalls[0].ToolName != "get_context" || string(toolCalls[0].Input) != `{"scope":"target"}` {
		t.Fatalf("tool call was not converted: %#v", toolCalls)
	}
}

func TestEinoChatModelFactorySanitizesToolNamesForModel(t *testing.T) {
	model := &fakeEinoToolCallingModel{
		response: schema.AssistantMessage("", []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "analytics_query_trends",
				Arguments: `{"query":"traffic"}`,
			},
		}}),
	}
	factory := EinoChatModelFactory{Model: model}
	nativeModel, err := factory.ResolveNativeModel(context.Background(), &ExecutionContext{}, []tools.Definition{{
		Name:        "analytics.query_trends",
		Description: "Query trends",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
	}})
	if err != nil {
		t.Fatalf("resolve native model: %v", err)
	}
	response, err := nativeModel.Generate(context.Background(), NativeModelRequest{
		Messages: []NativeMessage{
			{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "old-call", ToolName: "analytics.query_trends", Input: json.RawMessage(`{"query":"old"}`)}}},
			{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: "old-call", ToolName: "analytics.query_trends", Output: "ok"}}},
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(model.boundTools) != 1 || model.boundTools[0].Name != "analytics_query_trends" {
		t.Fatalf("expected sanitized tool binding, got %#v", model.boundTools)
	}
	if len(model.lastInput) < 2 || len(model.lastInput[0].ToolCalls) != 1 || model.lastInput[0].ToolCalls[0].Function.Name != "analytics_query_trends" {
		t.Fatalf("expected sanitized replay tool call, got %#v", model.lastInput)
	}
	if model.lastInput[1].ToolName != "analytics_query_trends" {
		t.Fatalf("expected sanitized replay tool result, got %#v", model.lastInput[1])
	}
	toolCalls := nativeToolCallBlocks(response.Message)
	if len(toolCalls) != 1 || toolCalls[0].ToolName != "analytics.query_trends" {
		t.Fatalf("tool call was not mapped back to runtime name: %#v", toolCalls)
	}
}

func TestEinoAgenticModelFactoryConvertsNativeRequests(t *testing.T) {
	model := &fakeEinoAgenticModel{
		response: &schema.AgenticMessage{
			Role: schema.AgenticRoleTypeAssistant,
			ContentBlocks: []*schema.ContentBlock{
				schema.NewContentBlock(&schema.AssistantGenText{Text: "need context"}),
				schema.NewContentBlock(&schema.FunctionToolCall{
					CallID:    "call-1",
					Name:      "get_context",
					Arguments: `{"scope":"target"}`,
				}),
			},
			ResponseMeta: &schema.AgenticResponseMeta{
				TokenUsage: &schema.TokenUsage{
					PromptTokens:     7,
					CompletionTokens: 11,
					PromptTokenDetails: schema.PromptTokenDetails{
						CachedTokens: 3,
					},
					CompletionTokensDetails: schema.CompletionTokensDetails{
						ReasoningTokens: 5,
					},
				},
				OpenAIExtension: &openaischema.ResponseMetaExtension{
					ID:                 "resp-1",
					PreviousResponseID: "resp-0",
				},
			},
		},
	}
	factory := EinoAgenticModelFactory{Model: model, Provider: "openai"}
	nativeModel, err := factory.ResolveNativeModel(context.Background(), &ExecutionContext{}, []tools.Definition{{
		Name:        "get_context",
		Description: "Get context",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"scope": map[string]any{"type": "string"}},
		},
	}})
	if err != nil {
		t.Fatalf("resolve agentic model: %v", err)
	}
	response, err := nativeModel.Generate(context.Background(), NativeModelRequest{
		SystemPrompt: "system",
		Messages:     []NativeMessage{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(model.lastInput) != 2 || model.lastInput[0].Role != schema.AgenticRoleTypeSystem || model.lastInput[1].Role != schema.AgenticRoleTypeUser {
		t.Fatalf("unexpected model input: %#v", model.lastInput)
	}
	if len(model.lastTools) != 1 || model.lastTools[0].Name != "get_context" {
		t.Fatalf("expected request tool option, got %#v", model.lastTools)
	}
	if response.Usage.InputTokens != 7 || response.Usage.CachedInputTokens != 3 || response.Usage.OutputTokens != 11 || response.Usage.ReasoningOutputTokens != 5 {
		t.Fatalf("usage was not converted: %#v", response.Usage)
	}
	if response.Continuation == nil || response.Continuation.Provider != "openai" || response.Continuation.ResponseID != "resp-1" || response.Continuation.PreviousResponseID != "resp-0" {
		t.Fatalf("continuation was not converted: %#v", response.Continuation)
	}
	toolCalls := nativeToolCallBlocks(response.Message)
	if len(toolCalls) != 1 || toolCalls[0].ToolName != "get_context" || string(toolCalls[0].Input) != `{"scope":"target"}` {
		t.Fatalf("tool call was not converted: %#v", toolCalls)
	}
}

func TestEinoAgenticModelFactorySanitizesToolNamesForResponsesAPI(t *testing.T) {
	model := &fakeEinoAgenticModel{
		response: &schema.AgenticMessage{
			Role: schema.AgenticRoleTypeAssistant,
			ContentBlocks: []*schema.ContentBlock{
				schema.NewContentBlock(&schema.FunctionToolCall{
					CallID:    "call-1",
					Name:      "analytics_query_trends",
					Arguments: `{"query":"traffic"}`,
				}),
			},
		},
	}
	factory := EinoAgenticModelFactory{Model: model, Provider: "openai"}
	nativeModel, err := factory.ResolveNativeModel(context.Background(), &ExecutionContext{}, []tools.Definition{{
		Name:        "analytics.query_trends",
		Description: "Query trends",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
	}})
	if err != nil {
		t.Fatalf("resolve agentic model: %v", err)
	}
	response, err := nativeModel.Generate(context.Background(), NativeModelRequest{
		Messages: []NativeMessage{
			{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "old-call", ToolName: "analytics.query_trends", Input: json.RawMessage(`{"query":"old"}`)}}},
			{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: "old-call", ToolName: "analytics.query_trends", Output: "ok"}}},
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(model.lastTools) != 1 || model.lastTools[0].Name != "analytics_query_trends" {
		t.Fatalf("expected sanitized request tool option, got %#v", model.lastTools)
	}
	if len(model.lastInput) < 2 || len(model.lastInput[0].ContentBlocks) != 1 {
		t.Fatalf("expected replay messages, got %#v", model.lastInput)
	}
	replayCall := model.lastInput[0].ContentBlocks[0].FunctionToolCall
	if replayCall == nil || replayCall.Name != "analytics_query_trends" {
		t.Fatalf("expected sanitized replay tool call, got %#v", model.lastInput[0].ContentBlocks[0])
	}
	replayResult := model.lastInput[1].ContentBlocks[0].FunctionToolResult
	if replayResult == nil || replayResult.Name != "analytics_query_trends" {
		t.Fatalf("expected sanitized replay tool result, got %#v", model.lastInput[1].ContentBlocks[0])
	}
	toolCalls := nativeToolCallBlocks(response.Message)
	if len(toolCalls) != 1 || toolCalls[0].ToolName != "analytics.query_trends" {
		t.Fatalf("tool call was not mapped back to runtime name: %#v", toolCalls)
	}
}

func TestEinoProviderFactoryDefaults(t *testing.T) {
	providerFactory := EinoProviderFactory{}
	provider, model := providerFactory.resolveProviderAndModel(&ExecutionContext{})
	if provider != "anthropic" || model != defaultNativeAnthropicModel {
		t.Fatalf("unexpected anthropic defaults provider=%q model=%q", provider, model)
	}
	provider, model = providerFactory.resolveProviderAndModel(&ExecutionContext{Agent: &agentcore.Agent{Provider: "openai"}})
	if provider != "openai" || model != defaultNativeOpenAIModel {
		t.Fatalf("unexpected openai defaults provider=%q model=%q", provider, model)
	}
	provider, model = providerFactory.resolveProviderAndModel(&ExecutionContext{Agent: &agentcore.Agent{Provider: "openrouter"}})
	if provider != "openrouter" || model != defaultNativeOpenRouterModel {
		t.Fatalf("unexpected openrouter defaults provider=%q model=%q", provider, model)
	}
	if got := resolveOpenAIResponsesBaseURL(""); got != defaultOpenAIResponsesBaseURL {
		t.Fatalf("expected default openai base url, got %q", got)
	}
	if got := resolveOpenRouterBaseURL(""); got != defaultOpenRouterBaseURL {
		t.Fatalf("expected default openrouter base url, got %q", got)
	}
}

func TestNativeMessagesToEinoCompactsLargeToolResultsForModel(t *testing.T) {
	output := strings.Repeat("line of file contents\n", 500)
	history := []NativeMessage{
		{Role: "assistant", Blocks: []NativeBlock{
			{Type: nativeBlockTypeToolCall, ToolCallID: "call-1", ToolName: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)},
		}},
		{Role: "tool", Blocks: []NativeBlock{
			{Type: nativeBlockTypeToolResult, ToolCallID: "call-1", ToolName: "read_file", Output: output},
		}},
	}

	msgs, err := nativeMessagesToEino("system prompt", history, nativeToolNameMapper{})
	if err != nil {
		t.Fatalf("native messages to eino: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected system plus assistant/tool replay messages, got %#v", msgs)
	}
	if !strings.Contains(msgs[2].Content, "truncated") {
		t.Fatalf("expected compacted tool output marker, got %q", msgs[2].Content)
	}
	if msgs[2].Content == output {
		t.Fatal("expected tool output to be compacted for model history")
	}
}

func TestNativeMessagesToAgenticCompactsLargeToolResultsForModel(t *testing.T) {
	output := strings.Repeat("line of file contents\n", 500)
	history := []NativeMessage{
		{Role: "assistant", Blocks: []NativeBlock{
			{Type: nativeBlockTypeToolCall, ToolCallID: "call-1", ToolName: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)},
		}},
		{Role: "tool", Blocks: []NativeBlock{
			{Type: nativeBlockTypeToolResult, ToolCallID: "call-1", ToolName: "read_file", Output: output},
		}},
	}

	msgs, err := nativeMessagesToAgentic("system prompt", history, nativeToolNameMapper{})
	if err != nil {
		t.Fatalf("native messages to agentic: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected system plus assistant/tool replay messages, got %#v", msgs)
	}
	resultBlocks := msgs[2].ContentBlocks[0].FunctionToolResult.Content
	if len(resultBlocks) != 1 || resultBlocks[0].Text == nil {
		t.Fatalf("expected text function tool result content, got %#v", resultBlocks)
	}
	if !strings.Contains(resultBlocks[0].Text.Text, "truncated") {
		t.Fatalf("expected compacted tool output marker, got %q", resultBlocks[0].Text.Text)
	}
	if resultBlocks[0].Text.Text == output {
		t.Fatal("expected tool output to be compacted for agentic model history")
	}
}

func TestNativeMessagesToEinoPreservesEmptyToolResultsWithPlaceholder(t *testing.T) {
	history := []NativeMessage{
		{Role: "assistant", Blocks: []NativeBlock{
			{Type: nativeBlockTypeToolCall, ToolCallID: "call-1", ToolName: "run_command", Input: json.RawMessage(`{"command":"mkdir -p tmp"}`)},
		}},
		{Role: "tool", Blocks: []NativeBlock{
			{Type: nativeBlockTypeToolResult, ToolCallID: "call-1", ToolName: "run_command", Output: ""},
		}},
	}

	msgs, err := nativeMessagesToEino("system prompt", history, nativeToolNameMapper{})
	if err != nil {
		t.Fatalf("native messages to eino: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected system plus assistant/tool replay messages, got %#v", msgs)
	}
	if msgs[2].Content != toolResultNoOutputPlaceholder {
		t.Fatalf("expected placeholder tool content, got %#v", msgs[2])
	}
}

func TestSanitizeNativeMessagesForReplayDropsOrphanedToolResults(t *testing.T) {
	history := []NativeMessage{
		{Role: "user", Content: "hello"},
		{Role: "tool", Blocks: []NativeBlock{
			{Type: nativeBlockTypeToolResult, ToolCallID: "orphan", ToolName: "read_file", Output: "orphan"},
		}},
		{Role: "assistant", Blocks: []NativeBlock{
			{Type: nativeBlockTypeToolCall, ToolCallID: "call-1", ToolName: "read_file"},
			{Type: nativeBlockTypeToolCall, ToolCallID: "call-2", ToolName: "read_file"},
		}},
		{Role: "tool", Blocks: []NativeBlock{
			{Type: nativeBlockTypeToolResult, ToolCallID: "call-1", ToolName: "read_file", Output: "matched"},
		}},
	}

	sanitized := sanitizeNativeMessagesForReplay(history)
	if len(sanitized) != 3 {
		t.Fatalf("expected user plus sanitized assistant/tool, got %#v", sanitized)
	}
	if sanitized[1].Role != "assistant" || len(nativeToolCallBlocks(sanitized[1])) != 1 || nativeToolCallBlocks(sanitized[1])[0].ToolCallID != "call-1" {
		t.Fatalf("expected only matched assistant tool call to remain, got %#v", sanitized[1])
	}
	if sanitized[2].Role != "tool" || sanitized[2].Blocks[0].ToolCallID != "call-1" {
		t.Fatalf("expected only matched tool result to remain, got %#v", sanitized[2])
	}
}

func TestNativeOutputCompactionExemptionKeepsBoundedAgentRuntimePayload(t *testing.T) {
	output := `{"_agent_runtime_compaction":{"exempt":true,"max_runes":10000},"content":"` + strings.Repeat("x", 5000) + `"}`
	visible := prepareNativeToolResultForModel("read_file", output, false)
	if visible.Compacted {
		t.Fatalf("expected bounded agent-runtime compaction exemption to be honored")
	}
	if visible.Content != output {
		t.Fatalf("expected exempt output to be unchanged")
	}
}

type fakeNativeFactory struct {
	model NativeModel
}

func (f fakeNativeFactory) ResolveNativeModel(ctx context.Context, execCtx *ExecutionContext, definitions []tools.Definition) (NativeModel, error) {
	return f.model, nil
}

type fakeNativeModel struct {
	responses []NativeModelResponse
	requests  []NativeModelRequest
}

func (m *fakeNativeModel) Generate(ctx context.Context, req NativeModelRequest) (*NativeModelResponse, error) {
	m.requests = append(m.requests, req)
	if len(m.responses) == 0 {
		return &NativeModelResponse{Message: NativeMessage{Role: "assistant", Content: "done"}}, nil
	}
	next := m.responses[0]
	m.responses = m.responses[1:]
	return &next, nil
}

type fakeEinoToolCallingModel struct {
	boundTools []*schema.ToolInfo
	lastInput  []*schema.Message
	response   *schema.Message
}

func (m *fakeEinoToolCallingModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	m.lastInput = append([]*schema.Message(nil), input...)
	return m.response, nil
}

func (m *fakeEinoToolCallingModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	m.lastInput = append([]*schema.Message(nil), input...)
	return nil, nil
}

func (m *fakeEinoToolCallingModel) WithTools(toolInfos []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	m.boundTools = append([]*schema.ToolInfo(nil), toolInfos...)
	return m, nil
}

type fakeEinoAgenticModel struct {
	lastInput []*schema.AgenticMessage
	lastTools []*schema.ToolInfo
	response  *schema.AgenticMessage
}

func (m *fakeEinoAgenticModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.AgenticMessage, error) {
	m.lastInput = append([]*schema.AgenticMessage(nil), input...)
	common := einomodel.GetCommonOptions(nil, opts...)
	m.lastTools = append([]*schema.ToolInfo(nil), common.Tools...)
	return m.response, nil
}

func (m *fakeEinoAgenticModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.lastInput = append([]*schema.AgenticMessage(nil), input...)
	common := einomodel.GetCommonOptions(nil, opts...)
	m.lastTools = append([]*schema.ToolInfo(nil), common.Tools...)
	return nil, nil
}
