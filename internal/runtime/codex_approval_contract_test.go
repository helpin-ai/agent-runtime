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
