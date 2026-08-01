package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestMissingCodexCompletionToolsAcceptsSuccessfulLogicalOrMCPCall(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{ID: "run-required-publish", AppID: "app-a"}
	agent := &agentcore.Agent{ExecutionConfig: json.RawMessage(`{"completion":{"required_tools":["publish_task_plan_doc"]}}`)}
	execCtx := &ExecutionContext{Store: mem, Run: run, Agent: agent}

	missing, err := missingCodexCompletionTools(ctx, execCtx)
	if err != nil || len(missing) != 1 || missing[0] != "publish_task_plan_doc" {
		t.Fatalf("expected missing publish tool, got missing=%v err=%v", missing, err)
	}
	if err := mem.AppendToolCall(ctx, &agentcore.ToolCall{
		AppID: run.AppID, RunID: run.ID, ToolName: "mcp__helpin__publish_task_plan_doc",
	}); err != nil {
		t.Fatalf("append successful MCP call: %v", err)
	}
	missing, err = missingCodexCompletionTools(ctx, execCtx)
	if err != nil || len(missing) != 0 {
		t.Fatalf("expected successful MCP call to satisfy logical contract, got missing=%v err=%v", missing, err)
	}
}

func TestMissingCodexCompletionToolsRejectsApprovalOnlyOrFailedCall(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{ID: "run-required-publish", AppID: "app-a"}
	agent := &agentcore.Agent{ExecutionConfig: json.RawMessage(`{"completion":{"required_tools":["publish_task_plan_doc"]}}`)}
	for _, call := range []agentcore.ToolCall{
		{AppID: run.AppID, RunID: run.ID, ToolName: "publish_task_plan_doc", ApprovalRequired: true},
		{AppID: run.AppID, RunID: run.ID, ToolName: "publish_task_plan_doc", Error: "missing content"},
	} {
		call := call
		if err := mem.AppendToolCall(ctx, &call); err != nil {
			t.Fatalf("append unsuccessful call: %v", err)
		}
	}
	missing, err := missingCodexCompletionTools(ctx, &ExecutionContext{Store: mem, Run: run, Agent: agent})
	if err != nil || len(missing) != 1 || missing[0] != "publish_task_plan_doc" {
		t.Fatalf("expected unsuccessful calls to leave contract missing, got missing=%v err=%v", missing, err)
	}
	if prompt := codexCompletionToolRetryPrompt(missing); !strings.Contains(prompt, "publish_task_plan_doc") || !strings.Contains(prompt, "do not restart") {
		t.Fatalf("unexpected corrective prompt: %q", prompt)
	}
}
