package runtime

import (
	"context"
	"fmt"
	"io"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func (m einoNativeModel) Summarize(ctx context.Context, prompt string, maxTokens int) (*NativeModelResponse, error) {
	// A non-nil empty slice overrides bound tools; nil means "use defaults"
	// in Eino providers. Per-call options leave the agent's bound model intact.
	response, err := m.model.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)}, einomodel.WithTools([]*schema.ToolInfo{}), einomodel.WithToolChoice(schema.ToolChoiceForbidden), einomodel.WithMaxTokens(maxTokens))
	if response == nil {
		return nil, err
	}
	incomplete := response.ResponseMeta == nil
	if response.ResponseMeta != nil {
		reason := response.ResponseMeta.FinishReason
		incomplete = reason != "stop" && reason != "end_turn" && reason != "stop_sequence"
	}
	return &NativeModelResponse{Message: einoMessageToNative(response, m.toolNames), Usage: nativeUsageFromEino(response), Incomplete: incomplete}, err
}

func (m einoAgenticNativeModel) Summarize(ctx context.Context, prompt string, maxTokens int) (*NativeModelResponse, error) {
	// The Responses adapter rejects the common ToolChoice option. No tools are
	// bound on that model; its normal tools are supplied only on agent requests.
	var response *schema.AgenticMessage
	var err error
	if m.provider == "openai_chatgpt" {
		reader, streamErr := m.model.Stream(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage(prompt)}, einomodel.WithTools([]*schema.ToolInfo{}))
		if streamErr != nil {
			return nil, streamErr
		}
		if reader == nil {
			return nil, fmt.Errorf("empty summary stream")
		}
		defer reader.Close()
		var chunks []*schema.AgenticMessage
		for {
			chunk, readErr := reader.Recv()
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return nil, readErr
			}
			chunks = append(chunks, chunk)
		}
		if len(chunks) == 0 {
			return nil, fmt.Errorf("empty summary response")
		}
		response, err = schema.ConcatAgenticMessages(chunks)
	} else {
		response, err = m.model.Generate(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage(prompt)}, einomodel.WithTools([]*schema.ToolInfo{}), einomodel.WithMaxTokens(maxTokens))
	}
	if response == nil {
		return nil, err
	}
	// This adapter currently uses Responses providers. Require explicit complete
	// metadata: a truncated summary must never replace the active transcript.
	incomplete := true
	if response.ResponseMeta != nil && response.ResponseMeta.OpenAIExtension != nil {
		ext := response.ResponseMeta.OpenAIExtension
		incomplete = string(ext.Status) != "completed" || ext.IncompleteDetails != nil
	}
	result := &NativeModelResponse{Message: agenticMessageToNative(response, m.toolNames), Usage: nativeUsageFromAgentic(response), Incomplete: incomplete}
	if incomplete && err == nil {
		err = fmt.Errorf("native summary provider did not report a complete response")
	}
	return result, err
}
