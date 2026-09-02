package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestCodexDynamicToolSpecsUseEffectiveRunAllowlist(t *testing.T) {
	execCtx, _ := newCodexDynamicToolTestContext(t, []string{"web_search", "fetch_url"}, []string{"fetch_url"})

	specs, err := codexDynamicToolSpecs(context.Background(), execCtx)
	if err != nil {
		t.Fatalf("build dynamic tool specs: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "fetch_url" {
		t.Fatalf("expected only fetch_url, got %#v", specs)
	}
	if specs[0].Type != "function" || !strings.Contains(string(specs[0].InputSchema), `"url"`) {
		t.Fatalf("unexpected dynamic tool spec: %#v", specs[0])
	}
}

func TestAppProviderToolIsProjectedToCodexAndNativeSDK(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	err := registry.ReplaceAppProvider("helpin", "helpin", 0, []tools.ProviderRegistration{{
		Definition: tools.Definition{Name: "create_collection", Description: "Create a collection.", InputSchema: map[string]any{"type": "object"}},
		Handler: func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"created":true}`), nil
		},
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	agent := &agentcore.Agent{AppID: "helpin", Name: "Ask Agent", RuntimeKind: agentcore.RuntimeCodex, AllowedTools: []string{"create_collection"}}
	if err := mem.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{AppID: "helpin", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "workspace", ID: "workspace-1"}, RuntimeKind: agentcore.RuntimeCodex, Status: agentcore.RunStatusRunning}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	execCtx := &ExecutionContext{Context: ctx, AppID: "helpin", Agent: agent, Run: run, Store: mem, Tools: registry, AllowedTools: registry.AllowedSetForApp("helpin", agent, nil)}
	specs, err := codexDynamicToolSpecs(ctx, execCtx)
	if err != nil || len(specs) != 1 || specs[0].Name != "create_collection" {
		t.Fatalf("Codex provider projection failed: specs=%#v err=%v", specs, err)
	}
	native := nativeAllowedToolDefinitions(execCtx)
	if len(native) != 1 || native[0].Name != "create_collection" {
		t.Fatalf("Native SDK provider projection failed: %#v", native)
	}
}

func TestNativeSDKRoundTripsPrefixedUsermavenToolName(t *testing.T) {
	const runtimeName = "usermaven__ai.get_task_status"
	mapper := newNativeToolNameMapper([]tools.Definition{{Name: runtimeName}})
	modelName := mapper.ModelName(runtimeName)
	if modelName != "usermaven__ai_get_task_status" {
		t.Fatalf("unexpected sanitized model name: %q", modelName)
	}
	if got := mapper.RuntimeName(modelName); got != runtimeName {
		t.Fatalf("native tool name did not round-trip: got %q want %q", got, runtimeName)
	}
}

func TestCodexWebSearchEnabledUsesEffectiveSearchPolicy(t *testing.T) {
	execCtx, _ := newCodexDynamicToolTestContext(t, []string{"web_search", "fetch_url"}, []string{"web_search"})
	if !codexWebSearchEnabled(execCtx) {
		t.Fatal("expected effective Exa permission to enable Codex web search")
	}
	if !strings.Contains((&CodexAdapter{}).codexDeveloperInstructions(execCtx, nil), "built-in web search") {
		t.Fatal("expected Codex search compatibility guidance")
	}

	execCtx, _ = newCodexDynamicToolTestContext(t, []string{"web_search", "fetch_url"}, []string{"fetch_url"})
	if codexWebSearchEnabled(execCtx) {
		t.Fatal("run-level tool narrowing must disable Codex web search")
	}
}

func TestCodexDynamicToolCallExecutesThroughGuardedGateway(t *testing.T) {
	execCtx, called := newCodexDynamicToolTestContext(t, []string{"fetch_url"}, nil)
	client := &fakeCodexRPC{}
	msg := codexRPCMessage{
		ID:     json.RawMessage(`41`),
		Method: "item/tool/call",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"fetch_url","arguments":{"url":"https://example.com/updates"}}`),
	}

	pause, err := (&CodexAdapter{}).handleCodexDynamicToolCall(context.Background(), client, execCtx, msg)
	if err != nil {
		t.Fatalf("handle dynamic tool call: %v", err)
	}
	if pause != nil {
		t.Fatalf("expected inline tool response, got pause %#v", pause)
	}
	if got := *called; !strings.Contains(got, "https://example.com/updates") {
		t.Fatalf("tool input was not executed: %q", got)
	}
	if len(client.responds) != 1 || !strings.Contains(client.responds[0], `"success":true`) || !strings.Contains(client.responds[0], `"type":"inputText"`) {
		t.Fatalf("unexpected Codex response: %#v", client.responds)
	}

	toolCalls, err := execCtx.Store.ListToolCalls(context.Background(), execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(toolCalls) != 1 || toolCalls[0].ToolName != "fetch_url" {
		t.Fatalf("expected guarded gateway audit record, got %#v", toolCalls)
	}
}

func TestCodexAppServerAdvertisesAndRunsDynamicTools(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
case "$line" in
  *'"config"'*'"features.default_mode_request_user_input":true'*'"web_search":"live"'*'"dynamicTools"'*'"fetch_url"'*) ;;
  *) printf '%s\n' 'thread/start did not enable default-mode user input, web search, and fetch_url dynamic tool' >&2; exit 2 ;;
esac
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' '{"id":4,"method":"item/tool/call","params":{"threadId":"thread-1","turnId":"turn-1","callId":"call-1","tool":"fetch_url","arguments":{"url":"https://example.com/updates"}}}'
IFS= read -r line
case "$line" in
  *'"id":4'*'"success":true'*'verified update'*) ;;
  *) printf '%s\n' 'dynamic tool response was invalid' >&2; exit 3 ;;
esac
printf '%s\n' '{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"msg-1","text":"digest created"}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	execCtx, called := newCodexDynamicToolTestContext(t, []string{"web_search", "fetch_url"}, nil)
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     3 * time.Second,
		AppServer:   true,
	})

	result, err := adapter.Execute(execCtx)
	if err != nil {
		t.Fatalf("execute Codex app-server: %v", err)
	}
	if result.AssistantMessage != "digest created" {
		t.Fatalf("unexpected assistant result: %q", result.AssistantMessage)
	}
	if !strings.Contains(*called, "https://example.com/updates") {
		t.Fatalf("dynamic tool was not executed: %q", *called)
	}
}

func TestCodexDynamicToolSpecsIncludeInteractionTools(t *testing.T) {
	execCtx, _ := newCodexDynamicToolTestContext(t, []string{"fetch_url", "request_approval", "request_user_input", "update_plan"}, nil)

	specs, err := codexDynamicToolSpecs(context.Background(), execCtx)
	if err != nil {
		t.Fatalf("build dynamic tool specs: %v", err)
	}
	names := make(map[string]bool, len(specs))
	for _, spec := range specs {
		names[spec.Name] = true
	}
	for _, want := range []string{"fetch_url", "request_approval", "request_user_input", "update_plan"} {
		if !names[want] {
			t.Fatalf("expected %s in dynamic tool specs, got %#v", want, names)
		}
	}
	for _, spec := range specs {
		if spec.Name == "request_approval" && !strings.Contains(string(spec.InputSchema), `"title"`) {
			t.Fatalf("expected request_approval schema, got %s", spec.InputSchema)
		}
	}
}

func TestCodexInteractionToolCallPausesForApproval(t *testing.T) {
	execCtx, _ := newCodexDynamicToolTestContext(t, []string{"request_approval"}, nil)
	client := &fakeCodexRPC{}
	msg := codexRPCMessage{
		ID:     json.RawMessage(`7`),
		Method: "item/tool/call",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","callId":"call-7","tool":"request_approval","arguments":{"phase":"task_doc","title":"Approve the task plan","summary":"Plan is ready."}}`),
	}

	pause, err := (&CodexAdapter{}).handleCodexDynamicToolCall(context.Background(), client, execCtx, msg)
	if err != nil {
		t.Fatalf("handle interaction tool call: %v", err)
	}
	if pause == nil || pause.Pending == nil {
		t.Fatalf("expected pause, got %#v", pause)
	}
	if pause.Pending.Kind != codexPendingRequestKindDynamicApproval || pause.Pending.Tool != "request_approval" {
		t.Fatalf("unexpected pending request: %#v", pause.Pending)
	}
	if pause.InteractionKind != "human_approval" || !strings.Contains(pause.Summary, "Plan is ready.") {
		t.Fatalf("unexpected pause metadata: %#v", pause)
	}
	if len(client.responds) != 0 {
		t.Fatalf("interaction pause must leave the tool call unanswered, got %#v", client.responds)
	}
	interactions, err := execCtx.Store.ListInteractions(context.Background(), execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != "approval_request" || interactions[0].Status != "pending" {
		t.Fatalf("expected pending approval_request interaction, got %#v", interactions)
	}
}

func TestCodexInteractionToolCallPausesForUserInput(t *testing.T) {
	execCtx, _ := newCodexDynamicToolTestContext(t, []string{"request_user_input"}, nil)
	client := &fakeCodexRPC{}
	msg := codexRPCMessage{
		ID:     json.RawMessage(`8`),
		Method: "item/tool/call",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","callId":"call-8","tool":"request_user_input","arguments":{"questions":[{"id":"q1","question":"Which region should the rollout target first?"}]}}`),
	}

	pause, err := (&CodexAdapter{}).handleCodexDynamicToolCall(context.Background(), client, execCtx, msg)
	if err != nil {
		t.Fatalf("handle interaction tool call: %v", err)
	}
	if pause == nil || pause.Pending == nil || pause.Pending.Kind != codexPendingRequestKindDynamicInput {
		t.Fatalf("expected user input pause, got %#v", pause)
	}
	if pause.InteractionKind != "human_input" {
		t.Fatalf("unexpected interaction kind: %q", pause.InteractionKind)
	}
	interactions, err := execCtx.Store.ListInteractions(context.Background(), execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != "request_user_input" {
		t.Fatalf("expected request_user_input interaction, got %#v", interactions)
	}
}

func TestCodexGatewayToolApprovalPausesRun(t *testing.T) {
	execCtx, called := newCodexDynamicToolTestContext(t, []string{"delete_record"}, nil)
	execCtx.Agent.ApprovalMode = agentcore.ApprovalModeMutatingTools
	if err := execCtx.Store.UpdateAgent(context.Background(), execCtx.Agent); err != nil {
		t.Fatalf("update agent: %v", err)
	}
	execCtx.Tools.Register(tools.Definition{
		Name:        "delete_record",
		Description: "Delete a record.",
		Mutating:    true,
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		*called = string(input)
		return json.RawMessage(`{"status":"deleted"}`), nil
	})
	execCtx.AllowedTools = tools.AllowedSet(execCtx.Agent, nil)
	client := &fakeCodexRPC{}
	msg := codexRPCMessage{
		ID:     json.RawMessage(`9`),
		Method: "item/tool/call",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","callId":"call-9","tool":"delete_record","arguments":{"record_id":"r-1"}}`),
	}

	pause, err := (&CodexAdapter{}).handleCodexDynamicToolCall(context.Background(), client, execCtx, msg)
	if err != nil {
		t.Fatalf("handle gateway tool call: %v", err)
	}
	if pause == nil || pause.Pending == nil || pause.Pending.Kind != codexPendingRequestKindDynamicGatewayApproval {
		t.Fatalf("expected gateway approval pause, got %#v", pause)
	}
	if *called != "" {
		t.Fatalf("tool must not run before approval, got %q", *called)
	}
	if len(client.responds) != 0 {
		t.Fatalf("gateway approval pause must leave the tool call unanswered, got %#v", client.responds)
	}
	interactions, err := execCtx.Store.ListInteractions(context.Background(), execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != "approval_request" {
		t.Fatalf("expected approval_request interaction, got %#v", interactions)
	}
}

func TestResolveCodexGatewayApprovalReplayExecutesApprovedTool(t *testing.T) {
	execCtx, called := newCodexDynamicToolTestContext(t, []string{"fetch_url"}, nil)
	execCtx.Run.ApprovalState = agentcore.ApprovalApproved
	if err := execCtx.Store.UpdateRun(context.Background(), execCtx.Run); err != nil {
		t.Fatalf("update run: %v", err)
	}
	params := json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","callId":"call-9","tool":"fetch_url","arguments":{"url":"https://example.com/x"}}`)
	pending := &codexPendingRequest{
		Kind:    codexPendingRequestKindDynamicGatewayApproval,
		Tool:    "fetch_url",
		Payload: params,
	}
	msg := codexRPCMessage{ID: json.RawMessage(`12`), Method: "item/tool/call", Params: params}

	response, err := (&CodexAdapter{}).resolveCodexGatewayApprovalReplay(context.Background(), execCtx, pending, msg, "approve", "", nil)
	if err != nil {
		t.Fatalf("resolve gateway approval replay: %v", err)
	}
	toolResponse, ok := response.(codexDynamicToolCallResponse)
	if !ok || !toolResponse.Success || len(toolResponse.ContentItems) == 0 || !strings.Contains(toolResponse.ContentItems[0].Text, "verified update") {
		t.Fatalf("expected executed tool result, got %#v", response)
	}
	if !strings.Contains(*called, "https://example.com/x") {
		t.Fatalf("approved tool was not executed: %q", *called)
	}
}

func TestResolveCodexGatewayApprovalReplayDeclines(t *testing.T) {
	execCtx, called := newCodexDynamicToolTestContext(t, []string{"fetch_url"}, nil)
	pending := &codexPendingRequest{
		Kind: codexPendingRequestKindDynamicGatewayApproval,
		Tool: "fetch_url",
	}

	response, err := (&CodexAdapter{}).resolveCodexGatewayApprovalReplay(context.Background(), execCtx, pending, codexRPCMessage{}, "request_changes", "Use the staging endpoint instead.", nil)
	if err != nil {
		t.Fatalf("resolve declined replay: %v", err)
	}
	toolResponse, ok := response.(codexDynamicToolCallResponse)
	if !ok || toolResponse.Success {
		t.Fatalf("expected declined tool response, got %#v", response)
	}
	if !strings.Contains(toolResponse.ContentItems[0].Text, "staging endpoint") {
		t.Fatalf("expected human feedback in tool result, got %#v", toolResponse.ContentItems)
	}
	if *called != "" {
		t.Fatalf("declined tool must not run, got %q", *called)
	}
}

func TestCodexResumeResponseForDynamicKinds(t *testing.T) {
	input, followup, err := codexResumeResponse(&codexPendingRequest{Kind: codexPendingRequestKindDynamicInput, Tool: "request_user_input"}, "", "Pick option A.", nil)
	if err != nil || followup != "" {
		t.Fatalf("dynamic input resume: %v followup=%q", err, followup)
	}
	inputResponse, ok := input.(codexDynamicToolCallResponse)
	if !ok || !inputResponse.Success || !strings.Contains(inputResponse.ContentItems[0].Text, "Pick option A.") {
		t.Fatalf("unexpected dynamic input response: %#v", input)
	}

	approval, followup, err := codexResumeResponse(&codexPendingRequest{Kind: codexPendingRequestKindDynamicApproval, Tool: "request_approval"}, "approve", "", nil)
	if err != nil || followup != "" {
		t.Fatalf("dynamic approval resume: %v followup=%q", err, followup)
	}
	approvalResponse, ok := approval.(codexDynamicToolCallResponse)
	if !ok || !approvalResponse.Success || !strings.Contains(approvalResponse.ContentItems[0].Text, "approved") {
		t.Fatalf("unexpected dynamic approval response: %#v", approval)
	}

	changes, _, err := codexResumeResponse(&codexPendingRequest{Kind: codexPendingRequestKindDynamicApproval, Tool: "request_approval"}, "request_changes", "Tighten the rollout plan.", nil)
	if err != nil {
		t.Fatalf("dynamic request_changes resume: %v", err)
	}
	changesResponse, ok := changes.(codexDynamicToolCallResponse)
	if !ok || !strings.Contains(changesResponse.ContentItems[0].Text, "Tighten the rollout plan.") {
		t.Fatalf("unexpected request_changes response: %#v", changes)
	}
}

func TestCodexRequiresAppServerForInteractionRuns(t *testing.T) {
	if codexRequiresAppServer(nil) {
		t.Fatal("nil context must not force the app server")
	}
	if codexRequiresAppServer(&ExecutionContext{}) {
		t.Fatal("a run without tools or interaction contracts must not force the app server")
	}
	if !codexRequiresAppServer(&ExecutionContext{AllowedTools: map[string]bool{"request_approval": true}}) {
		t.Fatal("allowed tools must force the app server")
	}
	if !codexRequiresAppServer(&ExecutionContext{SkillPolicy: skills.Policy{CompletionRequiresInteractionKinds: []string{"approval_request"}}}) {
		t.Fatal("completion interaction contracts must force the app server")
	}
}

func TestCodexDeveloperInstructionsAdvertiseCodexHomeSkillRoot(t *testing.T) {
	execCtx := &ExecutionContext{
		Agent:           &agentcore.Agent{SystemPrompt: "Plan the work."},
		StagedSkillRoot: "/lease/.agent-runtime/skills/agent-runtime",
	}
	state := &codexSessionState{CodexHome: "/runs/run-1/home/.codex"}

	instructions := (&CodexAdapter{}).codexDeveloperInstructions(execCtx, state)
	if !strings.Contains(instructions, "/runs/run-1/home/.codex/skills/agent-runtime") {
		t.Fatalf("expected codex home skill root in instructions, got %q", instructions)
	}
	if strings.Contains(instructions, "/lease/.agent-runtime/skills") {
		t.Fatalf("staging path must not leak into instructions, got %q", instructions)
	}
	if !strings.Contains(instructions, "Do not search for or construct skill paths inside the repository checkout.") {
		t.Fatalf("expected repository-checkout warning, got %q", instructions)
	}

	withoutState := (&CodexAdapter{}).codexDeveloperInstructions(execCtx, nil)
	if strings.Contains(withoutState, "/lease/.agent-runtime/skills") {
		t.Fatalf("staging path must never be advertised, got %q", withoutState)
	}
}

func TestCodexDeveloperInstructionsIncludeActiveSkillInstructions(t *testing.T) {
	execCtx := &ExecutionContext{
		Agent:             &agentcore.Agent{SystemPrompt: "Base prompt."},
		SkillInstructions: "Close the support coverage gap with the terminal tool.",
	}

	instructions := (&CodexAdapter{}).codexDeveloperInstructions(execCtx, nil)
	if !strings.Contains(instructions, "Active skill instructions:\nClose the support coverage gap with the terminal tool.") {
		t.Fatalf("active skill instructions missing from Codex prompt: %q", instructions)
	}
}

func TestCodexDeveloperInstructionsDoNotAdvertiseSkillPathsForSplitContract(t *testing.T) {
	execCtx := &ExecutionContext{
		Agent:           &agentcore.Agent{SystemPrompt: "Plan the work."},
		StagedSkillRoot: "/lease/.agent-runtime/skills",
		UsesSplitSkills: true,
	}
	state := &codexSessionState{CodexHome: "/runs/run-1/home/.codex"}
	instructions := (&CodexAdapter{}).codexDeveloperInstructions(execCtx, state)
	if strings.Contains(instructions, "skills/agent-runtime") || strings.Contains(instructions, "/lease/.agent-runtime/skills") {
		t.Fatalf("split contract must use dynamic skill tools, got %q", instructions)
	}
}

func newCodexDynamicToolTestContext(t *testing.T, agentTools, runTools []string) (*ExecutionContext, *string) {
	t.Helper()
	ctx := context.Background()
	mem := store.NewMemory()
	registry := tools.NewRegistry()
	called := ""
	registry.Register(tools.Definition{
		Name:        "fetch_url",
		Description: "Fetch and verify an exact URL.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{"type": "string"},
			},
			"required": []string{"url"},
		},
	}, func(_ context.Context, _ tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		called = string(input)
		return json.RawMessage(`{"status":"verified update"}`), nil
	})
	registry.Register(tools.Definition{
		Name:        "web_search",
		Description: "Search the web.",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ tools.CallContext, _ json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})
	agent := &agentcore.Agent{
		AppID:                 "app-a",
		Name:                  "Competitive digest",
		RuntimeKind:           agentcore.RuntimeCodex,
		AllowedTools:          append([]string(nil), agentTools...),
		ApprovalMode:          agentcore.ApprovalModeNever,
		DefaultInvocationMode: agentcore.InvocationAutonomous,
	}
	if err := mem.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:          "app-a",
		AgentID:        agent.ID,
		Target:         agentcore.TargetRef{Type: "workspace", ID: "workspace-1"},
		Input:          agentcore.RunInput{Instructions: "compile digest", AllowedTools: append([]string(nil), runTools...)},
		RuntimeKind:    agentcore.RuntimeCodex,
		ExecutionMode:  "lightweight",
		InvocationMode: agentcore.InvocationAutonomous,
		Status:         agentcore.RunStatusRunning,
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	allowed := tools.AllowedSet(agent, runTools)
	return &ExecutionContext{
		Context:      ctx,
		AppID:        "app-a",
		Agent:        agent,
		Run:          run,
		Store:        mem,
		AllowedTools: allowed,
		Tools:        registry,
	}, &called
}
