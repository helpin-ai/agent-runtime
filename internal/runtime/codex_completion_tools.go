package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func missingCodexCompletionTools(ctx context.Context, execCtx *ExecutionContext) ([]string, error) {
	if execCtx == nil || execCtx.Agent == nil || execCtx.Run == nil || execCtx.Store == nil {
		return nil, nil
	}
	required := codexRequiredCompletionTools(execCtx.Agent)
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

func codexRequiredCompletionTools(agent *agentcore.Agent) []string {
	if agent == nil || len(agent.ExecutionConfig) == 0 {
		return nil
	}
	var config struct {
		Completion struct {
			RequiredTools []string `json:"required_tools"`
		} `json:"completion"`
	}
	if json.Unmarshal(agent.ExecutionConfig, &config) != nil {
		return nil
	}
	seen := map[string]struct{}{}
	result := make([]string, 0, len(config.Completion.RequiredTools))
	for _, name := range config.Completion.RequiredTools {
		name = tools.CanonicalName(name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func codexCompletionToolRetryPrompt(missing []string) string {
	return "System correction: this run cannot complete until these tools have succeeded: " + strings.Join(missing, ", ") + ". Continue from the work already completed; do not restart repository inspection. Call each missing tool with its complete required payload. If a preview was drafted or published through another path, publish the full current artifact through the required tool now. Do not substitute a prose claim for the tool call."
}
