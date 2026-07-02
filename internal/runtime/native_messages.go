package runtime

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func persistNativeRunMessages(ctx context.Context, execCtx *ExecutionContext, result *nativeExecutionResult) (bool, error) {
	if execCtx == nil || execCtx.Store == nil || execCtx.Run == nil || result == nil {
		return false, nil
	}
	assistant := nativeLastAssistantMessage(result.Messages)
	if assistant != nil {
		message := &agentcore.AgentRunMessage{
			AppID:            execCtx.Run.AppID,
			RunID:            execCtx.Run.ID,
			RuntimeMessageID: result.AssistantMessageID,
			Role:             "assistant",
			Content:          nativePersistedMessageContent(result.AssistantText, assistant.Blocks),
			MessageType:      "assistant_turn",
			ContentBlocks:    marshalNativeBlocks(assistant.Blocks),
			ToolInvocations:  marshalNativeToolInvocations(result.ToolInvocations),
		}
		if strings.TrimSpace(message.Content) != "" || len(message.ContentBlocks) > 0 || len(message.ToolInvocations) > 0 {
			if err := execCtx.Store.AppendMessage(ctx, message); err != nil {
				return false, err
			}
		}
	}
	for _, toolMessage := range nativeFinalRoundToolMessages(result.Messages) {
		message := &agentcore.AgentRunMessage{
			AppID:         execCtx.Run.AppID,
			RunID:         execCtx.Run.ID,
			Role:          "tool",
			Content:       nativePersistedMessageContent(toolMessage.Content, toolMessage.Blocks),
			MessageType:   "tool_result",
			ContentBlocks: marshalNativeBlocks(toolMessage.Blocks),
		}
		if strings.TrimSpace(message.Content) == "" && len(message.ContentBlocks) == 0 {
			continue
		}
		if err := execCtx.Store.AppendMessage(ctx, message); err != nil {
			return false, err
		}
	}
	return true, nil
}

func nativeLastAssistantMessage(messages []NativeMessage) *NativeMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(messages[i].Role) == "assistant" {
			msg := messages[i]
			return &msg
		}
	}
	return nil
}

func nativeFinalRoundToolMessages(messages []NativeMessage) []NativeMessage {
	lastAssistantIndex := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.TrimSpace(messages[i].Role) == "assistant" {
			lastAssistantIndex = i
			break
		}
	}
	if lastAssistantIndex == -1 || lastAssistantIndex >= len(messages)-1 {
		return nil
	}
	out := make([]NativeMessage, 0, len(messages)-lastAssistantIndex-1)
	for _, message := range messages[lastAssistantIndex+1:] {
		if strings.TrimSpace(message.Role) == "tool" {
			out = append(out, message)
		}
	}
	return out
}

func nativePersistedMessageContent(fallback string, blocks []NativeBlock) string {
	if strings.TrimSpace(fallback) != "" {
		return strings.TrimSpace(fallback)
	}
	return nativePersistedContentFromBlocks(blocks)
}

func marshalNativeBlocks(blocks []NativeBlock) json.RawMessage {
	if len(blocks) == 0 {
		return nil
	}
	payload, err := json.Marshal(normalizeNativeBlocksForPersistence(blocks))
	if err != nil {
		return nil
	}
	return payload
}

func normalizeNativeBlocksForPersistence(blocks []NativeBlock) []NativeBlock {
	normalized := make([]NativeBlock, len(blocks))
	for i, block := range blocks {
		normalized[i] = block
		normalized[i].Type = strings.TrimSpace(block.Type)
		normalized[i].ToolName = strings.TrimSpace(block.ToolName)
		normalized[i].ToolCallID = strings.TrimSpace(block.ToolCallID)
		if normalized[i].Type == nativeBlockTypeToolCall {
			normalized[i].Input = normalizeNativeToolInput(block.Input)
		}
	}
	return normalized
}

func marshalNativeToolInvocations(invocations []nativeToolInvocation) json.RawMessage {
	if len(invocations) == 0 {
		return nil
	}
	payload, err := json.Marshal(invocations)
	if err != nil {
		return nil
	}
	return payload
}
