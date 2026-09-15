package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// nativeApprovalBlockState is the shape the runtime writes into a tool-result
// block's output when a mutating tool call pauses for human approval (see
// nativeRequestToolApproval). It is the durable record of "this call paused,
// unexecuted" that survives into run.OutputSummary and is replayed on resume.
type nativeApprovalBlockState struct {
	ApprovalRequired bool            `json:"approval_required"`
	InteractionID    string          `json:"interaction_id"`
	ToolName         string          `json:"tool_name"`
	ToolInputPreview json.RawMessage `json:"tool_input_preview"`
}

type nativeApprovalPlaceholder struct {
	MessageIndex int
	BlockIndex   int
	State        nativeApprovalBlockState
	GroupKey     string
}

// nativeReconcileResumedApprovals reconciles recorded pending-approval tool
// calls against their interaction decisions BEFORE the model sees the
// transcript again. It is the fix for the approval loop: an approved mutating
// call must execute (once) and its recorded result must replace the
// "approval_required" placeholder, so the model never re-emits the call.
//
// Correlation is by the tool_call_id/interaction_id already persisted in the
// transcript block — never a re-derived signature — so a replayed occurrence
// (block already carrying a real result) is cleanly distinct from a genuinely
// new identical call (a fresh block with no prior record).
//
// The transcript (run.OutputSummary.native_messages) is the single source of
// truth: once a block holds a real result it is left alone on every later
// attempt (replay), so a stale placeholder can never be re-fed to the model.
func nativeReconcileResumedApprovals(ctx context.Context, execCtx *ExecutionContext, messages []NativeMessage) ([]NativeMessage, error) {
	if execCtx == nil || execCtx.Store == nil || execCtx.Tools == nil || len(messages) == 0 {
		return messages, nil
	}
	// Only meaningful on resume; an initial run has no paused blocks.
	if _, ok := nativeLastResumePayload(execCtx); !ok {
		return messages, nil
	}

	interactions, err := execCtx.Store.ListInteractions(ctx, execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		return messages, fmt.Errorf("list approval interactions: %w", err)
	}
	byID := make(map[string]agentcore.AgentRunInteraction, len(interactions))
	for _, it := range interactions {
		byID[strings.TrimSpace(it.ID)] = it
	}

	placeholders := nativeApprovalPlaceholders(messages)
	if len(placeholders) == 0 {
		return messages, nil
	}
	latestByGroup := map[string]int{}
	for i, placeholder := range placeholders {
		latestByGroup[placeholder.GroupKey] = i
	}

	changed := false
	for i, placeholder := range placeholders {
		msg := &messages[placeholder.MessageIndex]
		block := &msg.Blocks[placeholder.BlockIndex]
		state := placeholder.State
		if latestByGroup[placeholder.GroupKey] != i {
			nativeSetToolResultBlock(msg, block, nativeSupersededApprovalResult(state), false)
			changed = true
			continue
		}

		decision, feedback := nativeApprovalDecision(execCtx, byID, state.InteractionID)
		if decision == "" {
			continue // not yet decided — leave paused
		}

		toolName := tools.CanonicalName(state.ToolName)
		input := state.ToolInputPreview
		if len(strings.TrimSpace(string(input))) == 0 {
			input = json.RawMessage(`{}`)
		}

		switch decision {
		case nativeDecisionApprove:
			if err := nativeBudgetCheck(ctx, execCtx, NativeContextPolicy{}, NativeUsage{}, 0, 0); err != nil {
				return messages, err
			}
			if _, ok := execCtx.Store.(agentcore.RunSummaryStore); !ok {
				return messages, fmt.Errorf("approval reconciliation requires summary-only persistence")
			}
			output, execErr := execCtx.Tools.Execute(ctx, toolCallContext(execCtx), toolName, input)
			text := strings.TrimSpace(string(output))
			isErr := execErr != nil
			if execErr != nil {
				text = execErr.Error()
			}
			if text == "" {
				text = "{}"
			}
			nativeSetToolResultBlock(msg, block, text, isErr)
			nativeRecordReconciledToolCall(ctx, execCtx, block, input, text, isErr)
			// Persist each known outcome before admitting another approved action.
			if err := nativePersistReconciledSummary(ctx, execCtx, messages); err != nil {
				return messages, err
			}
			slog.InfoContext(ctx, "approval reconcile: executed approved tool call",
				"run_id", execCtx.Run.ID, "tool_name", toolName,
				"tool_call_id", block.ToolCallID, "interaction_id", state.InteractionID, "is_error", isErr)
			changed = true
		case nativeDecisionRequestChanges:
			text := strings.TrimSpace(feedback)
			if text == "" {
				text = "Reviewer requested changes and did not approve this tool call. Do not retry the same action; revise your approach based on the run conversation."
			} else {
				text = "Reviewer requested changes instead of approving this tool call:\n" + text + "\n\nDo not retry the same action; revise your approach accordingly."
			}
			nativeSetToolResultBlock(msg, block, text, false)
			slog.InfoContext(ctx, "approval reconcile: change requested",
				"run_id", execCtx.Run.ID, "tool_name", toolName,
				"tool_call_id", block.ToolCallID, "interaction_id", state.InteractionID)
			changed = true
		}
	}

	if changed {
		if err := nativePersistReconciledSummary(ctx, execCtx, messages); err != nil {
			return messages, err
		}
	}
	return messages, nil
}

func nativeApprovalPlaceholders(messages []NativeMessage) []nativeApprovalPlaceholder {
	placeholders := []nativeApprovalPlaceholder{}
	for mi := range messages {
		for bi := range messages[mi].Blocks {
			block := messages[mi].Blocks[bi]
			if strings.TrimSpace(block.Type) != nativeBlockTypeToolResult {
				continue
			}
			state, ok := nativeParseApprovalBlock(block.Output)
			if !ok || !state.ApprovalRequired {
				continue // normal result, or already reconciled (branch 3: replay, do nothing)
			}
			placeholders = append(placeholders, nativeApprovalPlaceholder{
				MessageIndex: mi,
				BlockIndex:   bi,
				State:        state,
				GroupKey:     nativeApprovalGroupKey(state),
			})
		}
	}
	return placeholders
}

func nativeApprovalGroupKey(state nativeApprovalBlockState) string {
	input := state.ToolInputPreview
	if len(strings.TrimSpace(string(input))) == 0 {
		input = json.RawMessage(`{}`)
	}
	var decoded any
	if err := json.Unmarshal(input, &decoded); err == nil {
		if normalized, err := json.Marshal(decoded); err == nil {
			input = normalized
		}
	}
	return tools.CanonicalName(state.ToolName) + "\x00" + strings.TrimSpace(string(input))
}

func nativeSupersededApprovalResult(state nativeApprovalBlockState) string {
	toolName := tools.CanonicalName(state.ToolName)
	if toolName == "" {
		toolName = "this tool call"
	}
	return "This earlier duplicate approval request for " + toolName + " was superseded by a later approval request for the same tool input. No side effect was executed for this stale request."
}

// nativeParseApprovalBlock reports whether a tool-result block output is a
// pending-approval placeholder and extracts its correlation fields.
func nativeParseApprovalBlock(output string) (nativeApprovalBlockState, bool) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" || trimmed[0] != '{' {
		return nativeApprovalBlockState{}, false
	}
	var state nativeApprovalBlockState
	if err := json.Unmarshal([]byte(trimmed), &state); err != nil {
		return nativeApprovalBlockState{}, false
	}
	return state, true
}

const (
	nativeDecisionApprove        = "approve"
	nativeDecisionRequestChanges = "request_changes"
)

// nativeApprovalDecision resolves the decision for a specific interaction,
// reading both response_payload.decision and the intent (the runtime approve
// endpoint sends only intent; helpin's resume sends response_payload.decision).
// Falls back to the run's last_resume when the interaction lacks a payload.
func nativeApprovalDecision(execCtx *ExecutionContext, byID map[string]agentcore.AgentRunInteraction, interactionID string) (decision string, feedback string) {
	interactionID = strings.TrimSpace(interactionID)
	var (
		rawDecision string
		rawIntent   string
		content     string
	)
	if it, ok := byID[interactionID]; ok {
		// An unresolved interaction is not yet decided.
		if strings.TrimSpace(it.Status) == "pending" {
			return "", ""
		}
		d, i, c := nativeParseDecisionPayload(it.ResponsePayload)
		rawDecision, rawIntent, content = d, i, c
	}
	// Fallback to the run's last resume (covers the runtime approve endpoint,
	// which resolves the interaction with only an intent).
	if rawDecision == "" && rawIntent == "" {
		if resume, ok := nativeLastResumePayload(execCtx); ok {
			d, i, c := nativeParseDecisionPayload(resume.ResponsePayload)
			rawDecision, rawIntent = firstNonEmptyDecision(rawDecision, d), firstNonEmptyDecision(rawIntent, i)
			if content == "" {
				content = c
			}
			if rawIntent == "" {
				rawIntent = resume.Intent
			}
			if content == "" {
				content = resume.Content
			}
		}
	}
	return nativeNormalizeDecision(firstNonEmptyDecision(rawDecision, rawIntent)), content
}

func nativeParseDecisionPayload(payload json.RawMessage) (decision, intent, content string) {
	if len(strings.TrimSpace(string(payload))) == 0 {
		return "", "", ""
	}
	var body struct {
		Decision string `json:"decision"`
		Intent   string `json:"intent"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return "", "", ""
	}
	return strings.TrimSpace(body.Decision), strings.TrimSpace(body.Intent), strings.TrimSpace(body.Content)
}

func nativeNormalizeDecision(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "approve", "approved":
		return nativeDecisionApprove
	case "request_changes", "reject", "rejected", "request-changes":
		return nativeDecisionRequestChanges
	default:
		return ""
	}
}

func firstNonEmptyDecision(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// nativeSetToolResultBlock overwrites a reconciled tool-result block's output
// (and the message content mirror) with the authoritative result.
func nativeSetToolResultBlock(msg *NativeMessage, block *NativeBlock, output string, isErr bool) {
	block.Output = output
	block.IsError = isErr
	// The tool message's Content mirrors its single tool-result block output.
	if len(msg.Blocks) == 1 {
		msg.Content = output
	}
}

// nativePersistReconciledSummary rewrites native_messages in run.OutputSummary
// so a subsequent activity attempt replays the reconciled (real) results
// instead of the stale approval_required placeholders. Other summary fields
// are preserved.
func nativePersistReconciledSummary(ctx context.Context, execCtx *ExecutionContext, messages []NativeMessage) error {
	summary := map[string]any{}
	if len(execCtx.Run.OutputSummary) > 0 {
		_ = json.Unmarshal(execCtx.Run.OutputSummary, &summary)
	}
	summary["native_messages"] = publicNativeMessages(messages)
	encoded, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("marshal reconciled summary: %w", err)
	}
	execCtx.Run.OutputSummary = encoded
	store, ok := execCtx.Store.(agentcore.RunSummaryStore)
	if !ok {
		return fmt.Errorf("approval reconciliation requires summary-only persistence")
	}
	return store.UpdateRunOutputSummary(ctx, execCtx.AppID, execCtx.Run.ID, encoded)
}

// nativeRecordReconciledToolCall appends an audit tool-call row for a call that
// executed after approval, mirroring the normal execution path's audit record.
func nativeRecordReconciledToolCall(ctx context.Context, execCtx *ExecutionContext, block *NativeBlock, input json.RawMessage, output string, isErr bool) {
	errorText := ""
	if isErr {
		errorText = output
	}
	executed := nativeExecutedToolCall{
		ToolCallID: strings.TrimSpace(block.ToolCallID),
		ToolName:   strings.TrimSpace(block.ToolName),
		Input:      append(json.RawMessage(nil), input...),
		Output:     output,
		IsError:    isErr,
		Mutating:   true,
	}
	recordNativeToolCall(ctx, execCtx, executed, truncateNativeText(output, nativeToolSummaryLimit), errorText)
}
