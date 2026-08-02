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
)

func TestCodexAdapterRetriesMissingRequiredCompletionTool(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	retryInput := filepath.Join(tmp, "retry-input.json")
	script := `#!/bin/sh
[ "$1" = "app-server" ] || exit 77
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-required-tool","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-required-tool","turnId":"turn-1","itemId":"msg-1","delta":"I drafted the plan."}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-required-tool","turn":{"id":"turn-1","status":"completed"}}}'
IFS= read -r line
printf '%s' "$line" > "$RETRY_INPUT_PATH"
printf '%s\n' '{"id":4,"result":{"turn":{"id":"turn-2","status":"running"}}}'
printf '%s\n' '{"method":"item/started","params":{"threadId":"thread-required-tool","turnId":"turn-2","item":{"type":"mcpToolCall","id":"publish-1","server":"helpin","tool":"publish_task_plan_doc","arguments":{"content":"# Plan"},"status":"inProgress"}}}'
printf '%s\n' '{"method":"item/completed","params":{"threadId":"thread-required-tool","turnId":"turn-2","item":{"type":"mcpToolCall","id":"publish-1","server":"helpin","tool":"publish_task_plan_doc","arguments":{"content":"# Plan"},"status":"completed","result":{"status":"published"}}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-required-tool","turn":{"id":"turn-2","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Env:         []string{"RETRY_INPUT_PATH=" + retryInput},
		Timeout:     10 * time.Second,
		// AppServer is intentionally false. A required completion tool must
		// still select the bidirectional protocol automatically.
	})
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID: "run-required-tool", AppID: "app-a", RuntimeKind: agentcore.RuntimeCodex,
		Target: agentcore.TargetRef{Type: "task", ID: "T-1"},
		Input:  agentcore.RunInput{Instructions: "plan the task"},
	}
	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(), AppID: run.AppID, Store: mem, Run: run,
		Agent: &agentcore.Agent{
			Name: "Codex", Provider: "openai", Model: "gpt",
			ExecutionConfig: json.RawMessage(`{"completion":{"required_tools":["publish_task_plan_doc"]}}`),
		},
	})
	if err != nil {
		t.Fatalf("execute Codex adapter: %v", err)
	}
	if result == nil || result.AssistantMessage != "" {
		t.Fatalf("expected successful corrected completion, got %#v", result)
	}
	payload, err := os.ReadFile(retryInput)
	if err != nil {
		t.Fatalf("read corrective input: %v", err)
	}
	for _, snippet := range []string{"System correction", "publish_task_plan_doc", "do not restart"} {
		if !strings.Contains(string(payload), snippet) {
			t.Fatalf("expected corrective input to contain %q, got %s", snippet, payload)
		}
	}
	missing, err := missingCodexCompletionTools(context.Background(), &ExecutionContext{Store: mem, Run: run, Agent: &agentcore.Agent{ExecutionConfig: json.RawMessage(`{"completion":{"required_tools":["publish_task_plan_doc"]}}`)}})
	if err != nil || len(missing) != 0 {
		t.Fatalf("expected corrected MCP call to satisfy contract, got missing=%v err=%v", missing, err)
	}
}

func TestCodexAdapterSynthesizesMissingRequiredApprovalWithInteractionBroker(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-policy","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-policy","turnId":"turn-1","itemId":"msg-1","delta":"The plan is published. Please approve it."}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-policy","turn":{"id":"turn-1","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     10 * time.Second,
		AppServer:   true,
	})
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-policy-broker",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "task", ID: "T-1"},
		Input:       agentcore.RunInput{Instructions: "plan the task"},
		RuntimeKind: agentcore.RuntimeCodex,
	}
	result, err := adapter.Execute(&ExecutionContext{
		Context:           context.Background(),
		AppID:             run.AppID,
		Store:             mem,
		Agent:             &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run:               run,
		InteractionBroker: testInteractionBroker{store: mem, run: run},
		SkillPolicy: skills.Policy{
			CompletionRequiresInteractionKinds: []string{skills.InteractionKindApprovalRequest},
		},
		SkillDefinitions: []skills.Definition{{RequiredTools: []string{"publish_task_plan_doc"}}},
	})
	if err != nil {
		t.Fatalf("execute Codex adapter: %v", err)
	}
	if result == nil || !result.WaitForApproval || result.AssistantMessage != "The plan is published. Please approve it." {
		t.Fatalf("expected synthesized approval wait, got %#v", result)
	}
	interactions, err := mem.ListInteractions(context.Background(), run.AppID, run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != skills.InteractionKindApprovalRequest || interactions[0].Status != "pending" {
		t.Fatalf("expected pending synthesized approval, got %#v", interactions)
	}
	for _, snippet := range []string{`"phase":"task_doc"`, `"preview_panel_key":"task_plan_doc"`} {
		if !strings.Contains(string(interactions[0].RequestPayload), snippet) {
			t.Fatalf("expected approval payload to contain %s, got %s", snippet, interactions[0].RequestPayload)
		}
	}
	state, err := newCodexSessionStore(mem).Load(context.Background(), run.AppID, run.ID)
	if err != nil {
		t.Fatalf("load Codex session state: %v", err)
	}
	if state == nil || state.PendingInteraction == nil || state.PendingInteraction.ID != interactions[0].ID {
		t.Fatalf("expected pending interaction in resumable Codex state, got %#v", state)
	}
}

func TestCodexInteractionResumePromptRequiresRepublishAfterRequestedChanges(t *testing.T) {
	run := &agentcore.AgentRun{
		Input: agentcore.RunInput{Metadata: map[string]interface{}{
			"last_resume": map[string]interface{}{
				"intent":  "request_changes",
				"content": "Keep the plan on Kafka 3.x and add a rollback check.",
			},
		}},
	}
	prompt := codexInteractionResumePrompt(&ExecutionContext{Run: run}, &codexPendingInteraction{
		ID:   "interaction-1",
		Kind: skills.InteractionKindApprovalRequest,
	})
	for _, snippet := range []string{
		"The human requested changes",
		"republish the full replacement preview",
		"request approval again",
		"Keep the plan on Kafka 3.x and add a rollback check.",
	} {
		if !strings.Contains(prompt, snippet) {
			t.Fatalf("expected resume prompt to contain %q, got %q", snippet, prompt)
		}
	}
}

func TestCodexInteractionResumePromptRequiresSkillPostApprovalActions(t *testing.T) {
	run := &agentcore.AgentRun{
		Input: agentcore.RunInput{Metadata: map[string]interface{}{
			"last_resume": map[string]interface{}{
				"intent": "approve",
			},
		}},
	}
	prompt := codexInteractionResumePrompt(&ExecutionContext{Run: run}, &codexPendingInteraction{
		ID:   "interaction-1",
		Kind: skills.InteractionKindApprovalRequest,
	})
	for _, snippet := range []string{
		"approved the pending request",
		"every post-approval action required by the active skills",
		"do not stop with a prose-only acknowledgement",
	} {
		if !strings.Contains(prompt, snippet) {
			t.Fatalf("expected resume prompt to contain %q, got %q", snippet, prompt)
		}
	}
}

func TestCodexPlainTextQuestionPausesBeforeRequiredPublishToolEnforcement(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:             "run-scribe-question",
		AppID:          "app-a",
		RuntimeKind:    agentcore.RuntimeCodex,
		InvocationMode: agentcore.InvocationInteractive,
		Target:         agentcore.TargetRef{Type: "task", ID: "USE-479"},
		Input:          agentcore.RunInput{Instructions: "Upgrade kafka client"},
	}
	execCtx := &ExecutionContext{
		Context: ctx,
		AppID:   run.AppID,
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "Scribe", RuntimeKind: agentcore.RuntimeCodex},
		Run:     run,
		InteractionBroker: testInteractionBroker{
			store: mem,
			run:   run,
		},
		SkillPolicy: skills.Policy{
			CompletionRequiresInteractionKinds: []string{skills.InteractionKindApprovalRequest, skills.InteractionKindRequestUserInput},
			InteractionContracts: []skills.InteractionContract{{
				Kind: skills.InteractionKindRequestUserInput,
				Transports: map[string]skills.InteractionTransport{
					agentcore.RuntimeCodex: {Type: skills.TransportTypeRuntimeBridge},
				},
			}},
		},
		SkillDefinitions: []skills.Definition{{RequiredTools: []string{"publish_task_plan_doc"}}},
	}
	client := &fakeCodexRPC{next: []codexRPCMessage{
		{
			Method: "item/completed",
			Params: json.RawMessage(`{"threadId":"thread-scribe","turnId":"turn-scribe","item":{"type":"agentMessage","id":"msg-scribe","text":"The repository has Java, Rust, and Python Kafka clients. Which Kafka client should Task 479 upgrade, and to what version?"}}`),
		},
		{
			Method: "turn/completed",
			Params: json.RawMessage(`{"threadId":"thread-scribe","turn":{"id":"turn-scribe","status":"completed"}}`),
		},
	}}
	state := &codexSessionState{ThreadID: "thread-scribe"}

	result, err := (&CodexAdapter{}).collectCodexTurn(ctx, client, t.TempDir(), execCtx, state)
	if err != nil {
		t.Fatalf("collect Codex question turn: %v", err)
	}
	if !result.AwaitingInput || result.WaitForApproval {
		t.Fatalf("expected user-input pause before publish enforcement, got %#v", result)
	}
	if state.PendingInteraction == nil || state.PendingInteraction.Kind != skills.InteractionKindRequestUserInput {
		t.Fatalf("expected resumable input interaction state, got %#v", state)
	}
	interactions, err := mem.ListInteractions(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != skills.InteractionKindRequestUserInput || !strings.Contains(string(interactions[0].RequestPayload), "Which Kafka client") {
		t.Fatalf("unexpected synthesized input interaction: %#v", interactions)
	}
	if calls, err := mem.ListToolCalls(ctx, run.AppID, run.ID); err != nil || len(calls) != 0 {
		t.Fatalf("plain-text bridge must not invent a successful publish call: calls=%#v err=%v", calls, err)
	}
}

func TestCodexAdapterAutomaticallyUsesAppServerForRequiredInteraction(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
[ "$1" = "app-server" ] || exit 77
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-required-interaction","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-required-interaction","status":"running"}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-required-interaction","turn":{"id":"turn-required-interaction","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-required-interaction",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "task", ID: "T-1"},
		Input:       agentcore.RunInput{Instructions: "plan the task"},
		RuntimeKind: agentcore.RuntimeCodex,
	}
	if err := mem.AppendInteraction(context.Background(), &agentcore.AgentRunInteraction{
		AppID:           run.AppID,
		RunID:           run.ID,
		RuntimeKind:     run.RuntimeKind,
		InteractionKind: skills.InteractionKindApprovalRequest,
		Status:          "pending",
		Title:           "Approve task plan",
	}); err != nil {
		t.Fatalf("append interaction: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     2 * time.Second,
		// AppServer is intentionally false. The active skill contract must
		// select the app-server path automatically.
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   run.AppID,
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run:     run,
		SkillDefinitions: []skills.Definition{{
			Key:           "approval_protocol",
			RequiredTools: []string{"request_approval"},
		}},
	})
	if err != nil {
		t.Fatalf("execute app-server automatically: %v", err)
	}
	if !result.WaitForApproval {
		t.Fatalf("expected approval pause from automatically selected app-server, got %#v", result)
	}
}

func TestCodexCompletionInteractionKindsFallBackToRequiredTools(t *testing.T) {
	execCtx := &ExecutionContext{SkillDefinitions: []skills.Definition{
		{RequiredTools: []string{"mcp__agent_runtime__request_approval"}},
		{RequiredTools: []string{"request_user_input", "request_review_checkpoint"}},
		// A planning preview remains approval-gated even when a stale skill
		// projection omits request_approval and its policy metadata.
		{RequiredTools: []string{"publish_task_plan_doc"}},
	}}

	got := codexCompletionInteractionKinds(execCtx)
	want := []string{
		skills.InteractionKindApprovalRequest,
		skills.InteractionKindRequestUserInput,
		skills.InteractionKindReviewCheckpoint,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected required interaction kinds: got %v want %v", got, want)
	}
}

func TestCompletionApprovalContextForCustomTaskAgentStaysGeneric(t *testing.T) {
	phase, panelKey, title := CompletionApprovalContextForSkills(
		[]skills.Definition{{Key: "custom_deployment", RequiredTools: []string{"request_approval"}}},
		skills.InteractionKindApprovalRequest,
	)
	if phase != "approval" || panelKey != "" || title != "Approve agent result" {
		t.Fatalf("expected generic custom-agent approval context, got phase=%q panel=%q title=%q", phase, panelKey, title)
	}
}

func TestCompletionApprovalContextForAmbiguousPlanningAgentStaysGeneric(t *testing.T) {
	phase, panelKey, title := CompletionApprovalContextForSkills(
		[]skills.Definition{{RequiredTools: []string{"publish_prd_draft", "publish_task_plan"}}},
		skills.InteractionKindApprovalRequest,
	)
	if phase != "approval" || panelKey != "" || title != "Approve agent result" {
		t.Fatalf("expected generic ambiguous approval context, got phase=%q panel=%q title=%q", phase, panelKey, title)
	}
}
