package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/skills"
)

func latestPendingRuntimeInteraction(ctx context.Context, execCtx *ExecutionContext) (*codexPendingInteraction, bool, bool, error) {
	if execCtx == nil || execCtx.Store == nil || execCtx.Run == nil {
		return nil, false, false, nil
	}
	interactions, err := execCtx.Store.ListInteractions(ctx, execCtx.Run.AppID, execCtx.Run.ID)
	if err != nil {
		return nil, false, false, fmt.Errorf("list runtime interactions: %w", err)
	}
	for idx := len(interactions) - 1; idx >= 0; idx-- {
		interaction := interactions[idx]
		if strings.TrimSpace(interaction.Status) != "pending" {
			continue
		}
		// App-server approval/input interactions are paused immediately when the
		// JSON-RPC request arrives. They may still be pending in adapter-level
		// tests or eventually-consistent stores after the response is replayed,
		// so only MCP-created canonical interaction kinds are handled here.
		rawKind := strings.TrimSpace(interaction.InteractionKind)
		if rawKind == "human_input" || rawKind == "human_approval" {
			continue
		}
		kind := normalizeCodexRuntimeInteractionKind(interaction.InteractionKind)
		switch kind {
		case skills.InteractionKindRequestUserInput:
			return &codexPendingInteraction{ID: interaction.ID, Kind: kind}, false, true, nil
		case skills.InteractionKindApprovalRequest, skills.InteractionKindReviewCheckpoint:
			return &codexPendingInteraction{ID: interaction.ID, Kind: kind}, true, false, nil
		case "command_execution_approval", "file_change_approval", "permissions_approval":
			return &codexPendingInteraction{ID: interaction.ID, Kind: kind}, true, false, nil
		}
	}
	return nil, false, false, nil
}

func normalizeCodexRuntimeInteractionKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case "human_input", skills.InteractionKindRequestUserInput:
		return skills.InteractionKindRequestUserInput
	case "human_approval", skills.InteractionKindApprovalRequest:
		return skills.InteractionKindApprovalRequest
	default:
		return strings.TrimSpace(kind)
	}
}

func codexCompletionRequiresInteraction(execCtx *ExecutionContext) bool {
	return len(codexCompletionInteractionKinds(execCtx)) > 0
}

func codexCompletionInteractionKinds(execCtx *ExecutionContext) []string {
	if execCtx == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, kind := range skills.CompletionRequiredInteractionKinds(execCtx.SkillPolicy, execCtx.SkillDefinitions) {
		kind = normalizeCodexRuntimeInteractionKind(kind)
		if kind == "" {
			continue
		}
		seen[kind] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for kind := range seen {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func codexCompletionAllowedAfterApproval(execCtx *ExecutionContext) bool {
	intent, _, _ := lastResumePayload(execCtx)
	return strings.TrimSpace(intent) == "approve"
}

func codexCompletionInteractionRetryPrompt(execCtx *ExecutionContext) string {
	kinds := codexCompletionInteractionKinds(execCtx)
	return fmt.Sprintf(
		"System correction: the previous turn completed without the interaction required by the active skills. Continue from the work already completed; do not restart the task. Before completing, create one of these required interactions: %s. If an approval-ready preview was already published, republish the current full preview in this turn, then call mcp__agent_runtime__request_approval with the matching phase and preview_panel_key as the final action.",
		strings.Join(kinds, ", "),
	)
}

func codexInteractionResumePrompt(execCtx *ExecutionContext, pending *codexPendingInteraction) string {
	intent, content, responsePayload := lastResumePayload(execCtx)
	parts := []string{
		fmt.Sprintf("Continue after the human response to the pending %s interaction.", strings.TrimSpace(pending.Kind)),
	}
	switch strings.TrimSpace(intent) {
	case "approve":
		parts = append(parts, "The human approved the pending request. Continue from that approval without asking them to approve the same artifact again.")
	case "request_changes":
		parts = append(parts, "The human requested changes. Revise the current artifact, republish the full replacement preview, and request approval again when it is ready.")
	case "reply":
		parts = append(parts, "Use the human's reply to continue the current workflow.")
	default:
		if strings.TrimSpace(intent) != "" {
			parts = append(parts, "Resume intent: "+strings.TrimSpace(intent)+".")
		}
	}
	if strings.TrimSpace(content) != "" {
		parts = append(parts, "Human response:\n"+strings.TrimSpace(content))
	}
	if len(responsePayload) > 0 && strings.TrimSpace(string(responsePayload)) != "null" {
		var compact any
		if json.Unmarshal(responsePayload, &compact) == nil {
			if payload, err := json.Marshal(compact); err == nil {
				parts = append(parts, "Structured response: "+string(payload))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
