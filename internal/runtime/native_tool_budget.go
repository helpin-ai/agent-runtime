package runtime

import (
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Apply only when the run exposes durable retrieval. Small receipts stay intact;
// consumed large results and parallel result bursts keep previews plus references.
func nativeBudgetToolResults(messages []NativeMessage, names nativeToolNameMapper) []NativeMessage {
	if names.runtimeToModel[nativeToolReadResult] == "" {
		return messages
	}
	lastAssistant := -1
	for i := range messages {
		if messages[i].Role == "assistant" {
			lastAssistant = i
		}
	}
	out := append([]NativeMessage(nil), messages...)
	roundLimit := 16000
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role != "tool" {
			roundLimit = 16000
			continue
		}
		if i == len(out)-1 || out[i+1].Role != "tool" {
			count := 0
			for k := i; k >= 0 && out[k].Role == "tool"; k-- {
				for _, block := range out[k].Blocks {
					if block.Type == nativeBlockTypeToolResult {
						count++
					}
				}
			}
			roundLimit = max(512, 16000/max(1, count))
		}
		out[i].Blocks = append([]NativeBlock(nil), out[i].Blocks...)
		for j := range out[i].Blocks {
			block := &out[i].Blocks[j]
			if block.Type != nativeBlockTypeToolResult || block.ToolCallID == "" || tools.CanonicalName(block.ToolName) == "read_skill" {
				continue
			}
			visible := prepareNativeToolResultForModel(block.ToolName, block.Output, block.IsError)
			limit := roundLimit
			if i < lastAssistant {
				limit = min(limit, 2000)
			}
			if visible.VisibleRunes <= limit {
				continue
			}
			chars := []rune(block.Output)
			if len(chars) <= limit {
				continue
			}
			tail := min(300, limit/4)
			head := max(0, limit-tail-300)
			block.Output = string(chars[:head]) + fmt.Sprintf("\n[Saved output preview; full result: %s(tool_call_id=%q, offset=0, limit=2000). Do not repeat the original operation.]\n", names.ModelName(nativeToolReadResult), block.ToolCallID) + string(chars[len(chars)-tail:])
			out[i].Content = block.Output
		}
	}
	return out
}
