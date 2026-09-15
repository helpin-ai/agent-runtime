package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// NativeProviderState is internal checkpoint state, not displayable reasoning.
type NativeProviderState struct {
	Provider  string          `json:"provider"`
	Reasoning json.RawMessage `json:"reasoning"`
}

func withAgenticReasoning(message NativeMessage, source *schema.AgenticMessage, provider string) NativeMessage {
	if source == nil {
		return message
	}
	reasoning := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}
	for _, block := range source.ContentBlocks {
		if block != nil && block.Type == schema.ContentBlockTypeReasoning {
			reasoning.ContentBlocks = append(reasoning.ContentBlocks, block)
		}
	}
	if len(reasoning.ContentBlocks) > 0 {
		raw, _ := json.Marshal(reasoning)
		message.ProviderState = &NativeProviderState{Provider: provider, Reasoning: raw}
	}
	return message
}

func concatNativeProviderState(chunks []NativeModelResponse) (*NativeProviderState, error) {
	var messages []*schema.AgenticMessage
	provider := ""
	for _, chunk := range chunks {
		state := chunk.Message.ProviderState
		if state == nil {
			continue
		}
		if provider != "" && state.Provider != provider {
			return nil, fmt.Errorf("provider changed within a response")
		}
		provider = state.Provider
		var message schema.AgenticMessage
		if err := json.Unmarshal(state.Reasoning, &message); err != nil {
			return nil, err
		}
		messages = append(messages, &message)
	}
	if len(messages) == 0 {
		return nil, nil
	}
	combined, err := schema.ConcatAgenticMessages(messages)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(combined)
	return &NativeProviderState{Provider: provider, Reasoning: raw}, err
}

func nativeMessagesForProvider(system string, messages []NativeMessage, names nativeToolNameMapper, provider string) ([]*schema.AgenticMessage, error) {
	for _, message := range messages {
		if message.ProviderState != nil && message.ProviderState.Provider != provider {
			return nil, fmt.Errorf("checkpoint belongs to a different model provider; start a new run")
		}

		if message.ProviderState != nil {
			var state schema.AgenticMessage
			if err := json.Unmarshal(message.ProviderState.Reasoning, &state); err != nil {
				return nil, fmt.Errorf("invalid provider checkpoint state")
			}
			for _, block := range state.ContentBlocks {
				if block == nil || block.Type != schema.ContentBlockTypeReasoning {
					return nil, fmt.Errorf("invalid provider checkpoint block")
				}
			}
		}
	}
	return nativeMessagesToAgentic(system, messages, names)
}

func publicNativeMessages(messages []NativeMessage) []NativeMessage {
	out := append([]NativeMessage(nil), messages...)
	for i := range out {
		out[i].ProviderState = nil
	}
	return out
}
