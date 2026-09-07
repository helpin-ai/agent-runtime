package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

var errNativeAmbiguousTools = errors.New("native execution interrupted during tool execution; review recorded tool outcomes before starting a new run")

// Approval reconciliation also persists its completed outcomes in the run
// summary. A crash before the next private checkpoint must not discard them.
func (r *nativeRecorder) recoverApprovedTools() error {
	ctx := r.execCtx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	run, err := r.execCtx.Store.GetRun(ctx, r.execCtx.AppID, r.execCtx.Run.ID)
	if err != nil {
		return err
	}
	if run == nil {
		return errNativeAmbiguousTools
	}
	var persisted nativeOutputSummary
	if err := json.Unmarshal(run.OutputSummary, &persisted); err != nil {
		return errNativeAmbiguousTools
	}
	placeholders := nativeApprovalPlaceholders(r.state.Messages)
	if len(placeholders) == 0 {
		return errNativeAmbiguousTools
	}
	for _, pending := range placeholders {
		original := &r.state.Messages[pending.MessageIndex].Blocks[pending.BlockIndex]
		found := false
		for _, message := range persisted.Messages {
			for _, block := range message.Blocks {
				if block.Type != nativeBlockTypeToolResult || block.ToolCallID != original.ToolCallID || tools.CanonicalName(block.ToolName) != tools.CanonicalName(original.ToolName) || string(normalizeNativeToolInput(block.Input)) != string(normalizeNativeToolInput(original.Input)) {
					continue
				}
				if state, ok := nativeParseApprovalBlock(block.Output); ok && state.ApprovalRequired {
					continue
				}
				nativeSetToolResultBlock(&r.state.Messages[pending.MessageIndex], original, block.Output, block.IsError)
				found = true
			}
		}
		if !found {
			return errNativeAmbiguousTools
		}
	}
	r.state.Phase = "ready"
	return nil
}

// recoverTools does not replay side effects. Complete groups are safe to reuse;
// a missing read result becomes an explicit failed read which the model may retry.
func (r *nativeRecorder) recoverTools() error {
	messages := r.state.Messages
	start := len(messages) - 1
	for start >= 0 && messages[start].Role == "tool" {
		start--
	}
	if start < 0 || messages[start].Role != "assistant" {
		return errNativeAmbiguousTools
	}
	calls := nativeToolCallBlocks(messages[start])
	if len(calls) == 0 {
		return errNativeAmbiguousTools
	}
	results := map[string]bool{}
	for _, message := range messages[start+1:] {
		for _, block := range message.Blocks {
			if block.Type == nativeBlockTypeToolResult {
				results[block.ToolCallID] = true
			}
		}
	}
	// Old checkpoints used "tools" for approval execution as well. An approved
	// placeholder in that format is ambiguous, even though it resembles a result.
	if len(nativeApprovalPlaceholders(messages)) > 0 {
		if r.state.Result == nil || !r.state.Result.AwaitingApproval {
			return errNativeAmbiguousTools
		}
	}
	var missing []NativeMessage
	for _, call := range calls {
		if results[call.ToolCallID] {
			continue
		}
		name := tools.CanonicalName(call.ToolName)
		if r.execCtx.Tools == nil || nativeIsInteractionTool(name) || name == nativeToolFinishTurn {
			return errNativeAmbiguousTools
		}
		def, ok := r.execCtx.Tools.DefinitionForApp(r.execCtx.AppID, name)
		if !ok || def.Mutating {
			return errNativeAmbiguousTools
		}
		missing = append(missing, NativeMessage{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: call.ToolCallID, ToolName: name, IsError: true, Output: "Read interrupted before its result was saved. No result is available; retry this read if still needed."}}})
	}
	r.state.Messages = append(messages, missing...)
	r.state.Phase = "ready"
	if result := r.state.Result; result != nil && (result.TurnFinished || result.AwaitingInput || result.AwaitingApproval) {
		r.state.Phase = "done"
	}
	// Legacy finish_turn checkpoints lack the control flags, so do not continue
	// a turn that may already be finished without reconstructing its outcome.
	if r.state.Result == nil {
		for _, call := range calls {
			if strings.TrimSpace(call.ToolName) == nativeToolFinishTurn {
				return errNativeAmbiguousTools
			}
		}
	}
	return nil
}
