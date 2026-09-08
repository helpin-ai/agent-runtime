package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type EinoAgenticModelFactory struct {
	Model    einomodel.AgenticModel
	Provider string
}

func (f EinoAgenticModelFactory) ResolveNativeModel(ctx context.Context, execCtx *ExecutionContext, definitions []tools.Definition) (NativeModel, error) {
	if f.Model == nil {
		return nil, fmt.Errorf("eino agentic model is not configured")
	}
	toolNames := newNativeToolNameMapper(definitions)
	toolInfos, err := toEinoToolInfosWithNames(definitions, toolNames)
	if err != nil {
		return nil, err
	}
	return einoAgenticNativeModel{
		model:     f.Model,
		provider:  strings.TrimSpace(f.Provider),
		tools:     toolInfos,
		toolNames: toolNames,
	}, nil
}

type einoAgenticNativeModel struct {
	model     einomodel.AgenticModel
	provider  string
	tools     []*schema.ToolInfo
	toolNames nativeToolNameMapper
}

func (m einoAgenticNativeModel) Generate(ctx context.Context, req NativeModelRequest) (*NativeModelResponse, error) {
	messages, err := nativeMessagesToAgentic(req.SystemPrompt, req.Messages, m.toolNames)
	if err != nil {
		return nil, err
	}
	opts := []einomodel.Option{}
	if len(m.tools) > 0 {
		opts = append(opts, einomodel.WithTools(m.tools))
	}
	response, err := m.model.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	if response == nil {
		response = &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}
	}
	return &NativeModelResponse{
		Message:      agenticMessageToNative(response, m.toolNames),
		Usage:        nativeUsageFromAgentic(response),
		Continuation: providerContinuationFromAgenticMessage(strings.TrimSpace(m.provider), response),
	}, nil
}

func (m einoAgenticNativeModel) Stream(ctx context.Context, req NativeModelRequest) (NativeModelStream, error) {
	messages, err := nativeMessagesToAgentic(req.SystemPrompt, req.Messages, m.toolNames)
	if err != nil {
		return nil, err
	}
	opts := []einomodel.Option{}
	if len(m.tools) > 0 {
		opts = append(opts, einomodel.WithTools(m.tools))
	}
	reader, err := m.model.Stream(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, nil
	}
	return &einoAgenticNativeModelStream{
		reader:    reader,
		provider:  strings.TrimSpace(m.provider),
		toolNames: m.toolNames,
	}, nil
}

type einoAgenticNativeModelStream struct {
	reader    *schema.StreamReader[*schema.AgenticMessage]
	provider  string
	toolNames nativeToolNameMapper
}

func (s *einoAgenticNativeModelStream) Recv() (*NativeModelResponse, error) {
	if s == nil || s.reader == nil {
		return nil, io.EOF
	}
	message, err := s.reader.Recv()
	if err != nil {
		return nil, err
	}
	if message == nil {
		message = &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}
	}
	return &NativeModelResponse{
		Message:      agenticMessageChunkToNative(message, s.toolNames),
		Usage:        nativeUsageFromAgentic(message),
		Continuation: providerContinuationFromAgenticMessage(strings.TrimSpace(s.provider), message),
	}, nil
}

func (s *einoAgenticNativeModelStream) Close() {
	if s != nil && s.reader != nil {
		s.reader.Close()
	}
}

func nativeMessagesToAgentic(systemPrompt string, messages []NativeMessage, toolNames nativeToolNameMapper) ([]*schema.AgenticMessage, error) {
	messages = sanitizeNativeMessagesForReplay(messages)
	out := make([]*schema.AgenticMessage, 0, len(messages)+1)
	if strings.TrimSpace(systemPrompt) != "" {
		out = append(out, schema.SystemAgenticMessage(strings.TrimSpace(systemPrompt)))
	}
	for _, message := range messages {
		switch strings.TrimSpace(message.Role) {
		case "user":
			content := firstNonEmpty(strings.TrimSpace(message.Content), nativeMessageText(message))
			if content != "" {
				out = append(out, schema.UserAgenticMessage(content))
			}
		case "assistant":
			assistant := &schema.AgenticMessage{
				Role:          schema.AgenticRoleTypeAssistant,
				ContentBlocks: nativeBlocksToAgenticAssistantBlocks(message, toolNames),
			}
			if len(assistant.ContentBlocks) > 0 {
				out = append(out, assistant)
			}
		case "tool":
			for _, block := range message.Blocks {
				if strings.TrimSpace(block.Type) != nativeBlockTypeToolResult {
					continue
				}
				modelVisible := prepareNativeToolResultForModel(block.ToolName, block.Output, block.IsError)
				out = append(out, functionToolResultAgenticMessage(block.ToolCallID, toolNames.ModelName(block.ToolName), modelVisible.Content))
			}
			if len(message.Blocks) == 0 && strings.TrimSpace(message.Content) != "" {
				modelVisible := prepareNativeToolResultForModel("", message.Content, false)
				out = append(out, functionToolResultAgenticMessage("", "", modelVisible.Content))
			}
		case "":
			continue
		default:
			return nil, fmt.Errorf("unsupported native message role %q", message.Role)
		}
	}
	return out, nil
}

func nativeBlocksToAgenticAssistantBlocks(message NativeMessage, toolNames nativeToolNameMapper) []*schema.ContentBlock {
	blocks := make([]*schema.ContentBlock, 0, len(message.Blocks)+1)
	if len(message.Blocks) == 0 && strings.TrimSpace(message.Content) != "" {
		blocks = append(blocks, schema.NewContentBlock(&schema.AssistantGenText{Text: message.Content}))
	}
	for _, block := range message.Blocks {
		switch strings.TrimSpace(block.Type) {
		case nativeBlockTypeText:
			if strings.TrimSpace(block.Text) != "" {
				blocks = append(blocks, schema.NewContentBlock(&schema.AssistantGenText{Text: block.Text}))
			}
		case nativeBlockTypeToolCall:
			blocks = append(blocks, schema.NewContentBlock(&schema.FunctionToolCall{
				CallID:    strings.TrimSpace(block.ToolCallID),
				Name:      toolNames.ModelName(block.ToolName),
				Arguments: string(normalizeNativeToolInput(block.Input)),
			}))
		}
	}
	return blocks
}

func functionToolResultAgenticMessage(callID, name, content string) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeUser,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.FunctionToolResult{
				CallID: strings.TrimSpace(callID),
				Name:   strings.TrimSpace(name),
				Content: []*schema.FunctionToolResultContentBlock{{
					Type: schema.FunctionToolResultContentBlockTypeText,
					Text: &schema.UserInputText{Text: content},
				}},
			}),
		},
	}
}

func agenticMessageToNative(message *schema.AgenticMessage, toolNames nativeToolNameMapper) NativeMessage {
	return agenticMessageToNativeWithInputMode(message, toolNames, true)
}

func agenticMessageChunkToNative(message *schema.AgenticMessage, toolNames nativeToolNameMapper) NativeMessage {
	return agenticMessageToNativeWithInputMode(message, toolNames, false)
}

func agenticMessageToNativeWithInputMode(message *schema.AgenticMessage, toolNames nativeToolNameMapper, normalizeInput bool) NativeMessage {
	native := NativeMessage{Role: "assistant"}
	if message == nil {
		return native
	}
	// Stream chunks (normalizeInput=false) must keep text, reasoning, and
	// tool-argument fragments verbatim — per-chunk trimming deletes the
	// whitespace that sits on token boundaries. Whole messages keep the
	// whitespace-only filters as a cosmetic cleanup.
	keepText := func(text string) bool {
		if normalizeInput {
			return strings.TrimSpace(text) != ""
		}
		return text != ""
	}
	for _, block := range message.ContentBlocks {
		if block == nil {
			continue
		}
		switch block.Type {
		case schema.ContentBlockTypeAssistantGenText:
			if block.AssistantGenText != nil && keepText(block.AssistantGenText.Text) {
				native.Blocks = append(native.Blocks, NativeBlock{
					Type: nativeBlockTypeText,
					Text: block.AssistantGenText.Text,
				})
			}
		case schema.ContentBlockTypeReasoning:
			if block.Reasoning != nil && keepText(block.Reasoning.Text) {
				native.ReasoningContent += block.Reasoning.Text
			}
		case schema.ContentBlockTypeFunctionToolCall:
			if block.FunctionToolCall != nil {
				native.Blocks = append(native.Blocks, NativeBlock{
					Type:       nativeBlockTypeToolCall,
					ToolCallID: strings.TrimSpace(block.FunctionToolCall.CallID),
					ToolName:   toolNames.RuntimeName(block.FunctionToolCall.Name),
					Input:      nativeAgenticToolCallInput(block.FunctionToolCall.Arguments, normalizeInput),
				})
			}
		}
	}
	if normalizeInput {
		native.Content = nativeMessageText(native)
	} else {
		var text strings.Builder
		for _, block := range native.Blocks {
			if block.Type == nativeBlockTypeText {
				text.WriteString(block.Text)
			}
		}
		native.Content = text.String()
	}
	return native
}

func nativeAgenticToolCallInput(arguments string, normalizeInput bool) json.RawMessage {
	if !normalizeInput {
		// Chunk fragment: pass through verbatim so accumulated JSON keeps the
		// spaces inside its string values.
		if arguments == "" {
			return json.RawMessage(`{}`)
		}
		return json.RawMessage(arguments)
	}
	input := json.RawMessage(strings.TrimSpace(arguments))
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return normalizeNativeToolInput(input)
}

func nativeUsageFromAgentic(message *schema.AgenticMessage) NativeUsage {
	if message == nil || message.ResponseMeta == nil || message.ResponseMeta.TokenUsage == nil {
		return NativeUsage{}
	}
	usage := message.ResponseMeta.TokenUsage
	return NativeUsage{
		InputTokens:           int64(usage.PromptTokens),
		CachedInputTokens:     int64(usage.PromptTokenDetails.CachedTokens),
		OutputTokens:          int64(usage.CompletionTokens),
		ReasoningOutputTokens: int64(usage.CompletionTokensDetails.ReasoningTokens),
	}
}

func providerContinuationFromAgenticMessage(provider string, message *schema.AgenticMessage) *ProviderContinuation {
	if message == nil || message.ResponseMeta == nil || message.ResponseMeta.OpenAIExtension == nil {
		return nil
	}
	ext := message.ResponseMeta.OpenAIExtension
	if strings.TrimSpace(ext.ID) == "" {
		return nil
	}
	if provider == "" {
		provider = "openai"
	}
	return &ProviderContinuation{
		Provider:           provider,
		ResponseID:         strings.TrimSpace(ext.ID),
		PreviousResponseID: strings.TrimSpace(ext.PreviousResponseID),
	}
}
