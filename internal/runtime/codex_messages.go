package runtime

import (
	"context"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// persistCodexMapperMessage stores an intermediate Codex turn before a
// corrective turn reuses the same thread. The final turn is persisted by the
// engine from Result, but without this write the earlier streamed turn would
// disappear from the durable transcript.
func persistCodexMapperMessage(ctx context.Context, execCtx *ExecutionContext, mapper *codexEventMapper) error {
	if execCtx == nil || execCtx.Store == nil || execCtx.Run == nil || mapper == nil {
		return nil
	}
	content := mapper.AssistantText()
	toolInvocations := mapper.ToolInvocations()
	if strings.TrimSpace(content) == "" && len(toolInvocations) == 0 {
		return nil
	}
	return execCtx.Store.AppendMessage(ctx, &agentcore.AgentRunMessage{
		AppID:            execCtx.Run.AppID,
		RunID:            execCtx.Run.ID,
		RuntimeMessageID: mapper.AssistantMessageID(),
		Role:             "assistant",
		Content:          content,
		MessageType:      "assistant_turn",
		ToolInvocations:  toolInvocations,
	})
}
