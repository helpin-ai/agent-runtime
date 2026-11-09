package runtime

import (
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"unicode/utf8"
)

// Counts only; diagnostics must never duplicate prompts, CRM records or secrets.
func nativeEfficiencyMetrics(system string, messages []NativeMessage, defs []tools.Definition, input, generation, step int) map[string]any {
	names := newNativeToolNameMapper(defs)
	bounded := nativeBudgetToolResults(messages, names)
	raw, visible := 0, 0
	for _, m := range messages {
		for _, b := range m.Blocks {
			if b.Type == nativeBlockTypeToolResult {
				raw += utf8.RuneCountInString(b.Output)
			}
		}
	}
	for _, m := range bounded {
		for _, b := range m.Blocks {
			if b.Type == nativeBlockTypeToolResult {
				visible += utf8.RuneCountInString(nativeVisibleToolBlock(b, names).Content)
			}
		}
	}
	return map[string]any{"estimated_input_tokens": input, "generation": generation, "step": step, "tool_count": len(defs), "system_characters": utf8.RuneCountInString(system), "raw_tool_characters": raw, "visible_tool_characters": visible}
}
