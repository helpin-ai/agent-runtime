package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type EinoChatModelFactory struct {
	Model einomodel.ToolCallingChatModel
}

func (f EinoChatModelFactory) ResolveNativeModel(ctx context.Context, execCtx *ExecutionContext, definitions []tools.Definition) (NativeModel, error) {
	if f.Model == nil {
		return nil, fmt.Errorf("eino chat model is not configured")
	}
	toolNames := newNativeToolNameMapper(definitions)
	toolInfos, err := toEinoToolInfosWithNames(definitions, toolNames)
	if err != nil {
		return nil, err
	}
	modelWithTools, err := f.Model.WithTools(toolInfos)
	if err != nil {
		return nil, err
	}
	return einoNativeModel{model: modelWithTools, toolNames: toolNames}, nil
}

type einoNativeModel struct {
	model     einomodel.ToolCallingChatModel
	toolNames nativeToolNameMapper
}

func (m einoNativeModel) Generate(ctx context.Context, req NativeModelRequest) (*NativeModelResponse, error) {
	messages, err := nativeMessagesToEino(req.SystemPrompt, req.Messages, m.toolNames)
	if err != nil {
		return nil, err
	}
	response, err := m.model.Generate(ctx, messages)
	if err != nil {
		return nil, err
	}
	if response == nil {
		response = schema.AssistantMessage("", nil)
	}
	return &NativeModelResponse{
		Message: einoMessageToNative(response, m.toolNames),
		Usage:   nativeUsageFromEino(response),
	}, nil
}

func nativeMessagesToEino(systemPrompt string, messages []NativeMessage, toolNames nativeToolNameMapper) ([]*schema.Message, error) {
	messages = sanitizeNativeMessagesForReplay(messages)
	out := make([]*schema.Message, 0, len(messages)+1)
	if strings.TrimSpace(systemPrompt) != "" {
		out = append(out, schema.SystemMessage(strings.TrimSpace(systemPrompt)))
	}
	for _, message := range messages {
		switch strings.TrimSpace(message.Role) {
		case "user":
			content := firstNonEmpty(strings.TrimSpace(message.Content), nativeMessageText(message))
			if content != "" {
				out = append(out, schema.UserMessage(content))
			}
		case "assistant":
			out = append(out, schema.AssistantMessage(firstNonEmpty(strings.TrimSpace(message.Content), nativeMessageText(message)), nativeBlocksToEinoToolCalls(message.Blocks, toolNames)))
		case "tool":
			for _, block := range message.Blocks {
				if strings.TrimSpace(block.Type) != nativeBlockTypeToolResult {
					continue
				}
				modelVisible := prepareNativeToolResultForModel(block.ToolName, block.Output, block.IsError)
				out = append(out, schema.ToolMessage(modelVisible.Content, strings.TrimSpace(block.ToolCallID), schema.WithToolName(toolNames.ModelName(block.ToolName))))
			}
			if len(message.Blocks) == 0 && strings.TrimSpace(message.Content) != "" {
				modelVisible := prepareNativeToolResultForModel("", message.Content, false)
				out = append(out, schema.ToolMessage(modelVisible.Content, "", schema.WithToolName("")))
			}
		case "":
			continue
		default:
			return nil, fmt.Errorf("unsupported native message role %q", message.Role)
		}
	}
	return out, nil
}

func nativeBlocksToEinoToolCalls(blocks []NativeBlock, toolNames nativeToolNameMapper) []schema.ToolCall {
	out := make([]schema.ToolCall, 0, len(blocks))
	for _, block := range blocks {
		if strings.TrimSpace(block.Type) != nativeBlockTypeToolCall {
			continue
		}
		args := normalizeNativeToolInput(block.Input)
		out = append(out, schema.ToolCall{
			ID:   strings.TrimSpace(block.ToolCallID),
			Type: "function",
			Function: schema.FunctionCall{
				Name:      toolNames.ModelName(block.ToolName),
				Arguments: string(args),
			},
		})
	}
	return out
}

func einoMessageToNative(message *schema.Message, toolNames nativeToolNameMapper) NativeMessage {
	native := NativeMessage{
		Role:    "assistant",
		Content: strings.TrimSpace(message.Content),
	}
	if strings.TrimSpace(message.Content) != "" {
		native.Blocks = append(native.Blocks, NativeBlock{Type: nativeBlockTypeText, Text: message.Content})
	}
	for _, toolCall := range message.ToolCalls {
		input := json.RawMessage(strings.TrimSpace(toolCall.Function.Arguments))
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		native.Blocks = append(native.Blocks, NativeBlock{
			Type:       nativeBlockTypeToolCall,
			ToolCallID: strings.TrimSpace(toolCall.ID),
			ToolName:   toolNames.RuntimeName(toolCall.Function.Name),
			Input:      normalizeNativeToolInput(input),
		})
	}
	if native.Content == "" {
		native.Content = nativeMessageText(native)
	}
	return native
}

func nativeUsageFromEino(message *schema.Message) NativeUsage {
	if message == nil || message.ResponseMeta == nil || message.ResponseMeta.Usage == nil {
		return NativeUsage{}
	}
	usage := message.ResponseMeta.Usage
	return NativeUsage{
		InputTokens:           int64(usage.PromptTokens),
		CachedInputTokens:     int64(usage.PromptTokenDetails.CachedTokens),
		OutputTokens:          int64(usage.CompletionTokens),
		ReasoningOutputTokens: int64(usage.CompletionTokensDetails.ReasoningTokens),
	}
}
