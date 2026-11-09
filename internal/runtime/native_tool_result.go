package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const nativeToolReadResult = "read_tool_result"

func nativeReadResultDefinition() tools.Definition {
	return tools.Definition{Name: nativeToolReadResult, Description: "Read a saved tool result from this run without repeating the original operation. Offsets and limits count Unicode characters; maximum page size 4000.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"tool_call_id": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 0}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 4000}}, "required": []string{"tool_call_id"}, "additionalProperties": false}}
}

func nativeReadToolResult(ctx context.Context, x *ExecutionContext, call NativeBlock) nativeExecutedToolCall {
	result := nativeExecutedToolCall{ToolCallID: call.ToolCallID, ToolName: nativeToolReadResult, Input: call.Input}
	output, err := nativeSavedToolResult(ctx, x, call.Input)
	result.Output, result.IsError = output, err != nil
	if err != nil {
		result.Output = err.Error()
	}
	return result
}

func nativeSavedToolResult(ctx context.Context, x *ExecutionContext, input json.RawMessage) (string, error) {
	var req struct {
		ID     string `json:"tool_call_id"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(input)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return "", fmt.Errorf("invalid saved-result request")
	}
	if req.ID == "" || req.Offset < 0 || req.Limit < 0 || req.Limit > 4000 {
		return "", fmt.Errorf("supply a tool_call_id, nonnegative offset, and limit between 1 and 4000")
	}
	if req.Limit == 0 {
		req.Limit = 2000
	}
	calls, err := x.Store.ListToolCalls(ctx, x.AppID, x.Run.ID)
	if err != nil {
		return "", fmt.Errorf("cannot load saved tool results: %w", err)
	}
	for i := len(calls) - 1; i >= 0; i-- {
		var saved struct {
			ID     string `json:"tool_call_id"`
			Output string `json:"output"`
		}
		if json.Unmarshal(calls[i].Output, &saved) != nil || saved.ID != req.ID {
			continue
		}
		chars := []rune(saved.Output)
		if req.Offset > len(chars) {
			return "", fmt.Errorf("offset exceeds result length %d", len(chars))
		}
		end := min(len(chars), req.Offset+req.Limit)
		page := map[string]any{"tool_call_id": req.ID, "tool_name": calls[i].ToolName, "is_error": calls[i].Error != "", "offset": req.Offset, "total_characters": len(chars), "content": string(chars[req.Offset:end])}
		if end < len(chars) {
			page["next_offset"] = end
		}
		emitNativeEvent(ctx, x, "context.saved_result_read", map[string]any{"tool_call_id": req.ID, "offset": req.Offset, "returned_characters": end - req.Offset, "total_characters": len(chars)})
		raw, err := json.Marshal(page)
		return string(raw), err
	}
	return "", fmt.Errorf("no saved result for that tool_call_id in this run; do not repeat a mutation to recover output")
}

func nativeVisibleToolBlock(block NativeBlock, names nativeToolNameMapper) nativeModelVisibleToolOutput {
	visible := prepareNativeToolResultForModel(block.ToolName, block.Output, block.IsError)
	if visible.Compacted && block.ToolCallID != "" && names.runtimeToModel[nativeToolReadResult] != "" {
		visible.Content += fmt.Sprintf("\nFull output saved: use %s with tool_call_id=%q and a bounded offset/limit. Do not repeat the original operation.", names.ModelName(nativeToolReadResult), block.ToolCallID)
	}
	return visible
}

func nativeSavedResultsEnabled(x *ExecutionContext) bool {
	if x == nil || x.Store == nil {
		return false
	}
	p, err := nativeContextPolicy(x)
	return err == nil && p.Enabled
}

// A checkpoint can survive an audit-row write failure. Repair missing rows from
// that checkpoint before exposing references or allowing compaction to retire it.
func nativeEnsureSavedToolResults(ctx context.Context, x *ExecutionContext, messages []NativeMessage) error {
	if !nativeSavedResultsEnabled(x) {
		return nil
	}
	calls, err := x.Store.ListToolCalls(ctx, x.AppID, x.Run.ID)
	if err != nil {
		return err
	}
	saved := map[string]bool{}
	for _, call := range calls {
		var row struct {
			ID string `json:"tool_call_id"`
		}
		if json.Unmarshal(call.Output, &row) == nil {
			saved[row.ID] = true
		}
	}
	for _, message := range messages {
		for _, block := range message.Blocks {
			if block.Type != nativeBlockTypeToolResult || block.ToolCallID == "" || saved[block.ToolCallID] {
				continue
			}
			errorText := ""
			if block.IsError {
				errorText = block.Output
			}
			mutating := false
			if x.Tools != nil {
				if def, ok := x.Tools.DefinitionForApp(x.AppID, block.ToolName); ok {
					mutating = def.Mutating
				}
			}
			if err := recordNativeToolCall(ctx, x, nativeExecutedToolCall{ToolCallID: block.ToolCallID, ToolName: block.ToolName, Input: block.Input, Output: block.Output, IsError: block.IsError, Mutating: mutating}, truncateNativeText(block.Output, nativeToolSummaryLimit), errorText); err != nil {
				return err
			}
			saved[block.ToolCallID] = true
		}
	}
	return nil
}
