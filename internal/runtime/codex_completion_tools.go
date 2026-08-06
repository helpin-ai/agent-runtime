package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/completion"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func missingCodexCompletionTools(ctx context.Context, execCtx *ExecutionContext) ([]string, error) {
	if execCtx == nil || execCtx.Agent == nil || execCtx.Run == nil || execCtx.Store == nil {
		return nil, nil
	}
	required := codexRequiredCompletionTools(execCtx)
	if len(required) == 0 {
		return nil, nil
	}
	calls, err := execCtx.Store.ListToolCalls(ctx, execCtx.Run.AppID, execCtx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("list Codex completion tool calls: %w", err)
	}
	succeeded := make(map[string]bool, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.Error) != "" || call.ApprovalRequired {
			continue
		}
		if name := tools.CanonicalName(call.ToolName); name != "" {
			succeeded[name] = true
		}
	}
	missing := make([]string, 0, len(required))
	for _, toolName := range required {
		if !succeeded[toolName] {
			missing = append(missing, toolName)
		}
	}
	return missing, nil
}

func codexRequiredCompletionTools(execCtx *ExecutionContext) []string {
	if execCtx == nil {
		return nil
	}
	return completion.RequiredTools(execCtx.Agent, execCtx.Run)
}

func codexCompletionToolRetryPrompt(missing []string) string {
	return "System correction: this run cannot complete until these tools have succeeded: " + strings.Join(missing, ", ") + ". Continue from the work already completed; do not restart repository inspection. Call each missing tool with its complete required payload. If a preview was drafted or published through another path, publish the full current artifact through the required tool now. Do not substitute a prose claim for the tool call."
}
