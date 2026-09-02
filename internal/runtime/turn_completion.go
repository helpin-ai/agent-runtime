package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const nativeToolFinishTurn = "finish_turn"

const turnCompletionGuardErrorCode = "turn_completion_guard_exhausted"

type nativeFinishTurnRequest struct {
	Outcome string `json:"outcome"`
	Summary string `json:"summary"`
	Blocker string `json:"blocker,omitempty"`
}

func explicitTurnCompletionEnabled(execCtx *ExecutionContext) bool {
	return execCtx != nil && agentcore.RequiresExplicitTurnFinish(execCtx.Run)
}

func turnCompletionMaxCorrections(execCtx *ExecutionContext) int {
	if !explicitTurnCompletionEnabled(execCtx) {
		return 0
	}
	return agentcore.NormalizeTurnPolicy(execCtx.Run.Input.TurnPolicy).MaxCompletionCorrections
}

func nativeFinishTurnToolDefinition() tools.Definition {
	return tools.Definition{
		Name:        nativeToolFinishTurn,
		Description: "Explicitly finish the current assistant turn after all requested work is complete, or report that progress is blocked. This must be the only tool call in the response. The summary must contain the complete user-facing outcome because the runtime uses it when a provider emits a tool-call-only response.",
		Category:    "Runtime control",
		Mutating:    false,
		RiskLevel:   tools.RiskLevelRead,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"outcome": map[string]any{
					"type":        "string",
					"enum":        []string{"completed", "blocked"},
					"description": "Use completed only when the requested work is actually done. Use blocked only when the run cannot continue without human input or an external-state change.",
				},
				"summary": map[string]any{
					"type":        "string",
					"description": "Complete user-facing outcome for this turn. It must stand on its own when the provider emits no separate assistant text.",
				},
				"blocker": map[string]any{
					"type":        "string",
					"description": "Required when outcome is blocked; explain exactly what is needed to continue.",
				},
			},
			"required":             []string{"outcome", "summary"},
			"additionalProperties": false,
		},
	}
}

func nativeTurnCompletionInstructions() string {
	return "Turn completion contract: this run uses explicit completion. Continue working through ordinary tool calls until the current request is genuinely complete. Do not stop after promising or describing future work. When complete, call finish_turn as the only tool call in that response with outcome=completed and put the complete user-facing answer in summary. Also write the final answer as ordinary assistant text when the provider supports text alongside a tool call; the runtime uses summary as the answer when the provider emits a tool-call-only response. If progress genuinely cannot continue without human input or an external-state change, prefer request_user_input for a specific answer; otherwise call finish_turn alone with outcome=blocked and a concrete blocker. A response without a valid finish_turn call does not end the turn and will be returned for correction."
}

func nativeTurnCompletionCorrection(attempt, maximum int, reason string) NativeMessage {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "the response did not explicitly finish the turn"
	}
	return NativeMessage{
		Role: "user",
		Content: fmt.Sprintf(
			"Runtime completion correction %d/%d: %s. Continue the existing work from the transcript; do not restart discovery or merely promise the next action. When the request is actually complete, call %s as the only tool call in that response and put the complete user-facing answer in its summary; include ordinary assistant text too when supported. If genuinely blocked, use a supported interaction tool or call %s with outcome=blocked and a concrete blocker.",
			attempt,
			maximum,
			reason,
			nativeToolFinishTurn,
			nativeToolFinishTurn,
		),
	}
}

func turnCompletionGuardExhausted(maximum int, reason string) error {
	return fmt.Errorf("%s: model failed to explicitly finish the turn after %d correction attempts: %s", turnCompletionGuardErrorCode, maximum, strings.TrimSpace(reason))
}

func executeNativeFinishTurn(toolCall NativeBlock) nativeExecutedToolCall {
	input := normalizeNativeToolInput(toolCall.Input)
	executed := nativeExecutedToolCall{
		ToolCallID: strings.TrimSpace(toolCall.ToolCallID),
		ToolName:   nativeToolFinishTurn,
		Input:      input,
		Mutating:   false,
	}
	if toolCall.FinishRejected {
		executed.IsError = true
		executed.Output = "finish_turn must be the only tool call in the assistant response"
		return executed
	}
	var req nativeFinishTurnRequest
	if err := json.Unmarshal(input, &req); err != nil {
		executed.IsError = true
		executed.Output = "invalid finish_turn input: " + err.Error()
		return executed
	}
	req.Outcome = strings.TrimSpace(req.Outcome)
	req.Summary = strings.TrimSpace(req.Summary)
	req.Blocker = strings.TrimSpace(req.Blocker)
	if req.Outcome != "completed" && req.Outcome != "blocked" {
		executed.IsError = true
		executed.Output = "finish_turn outcome must be completed or blocked"
		return executed
	}
	if req.Summary == "" {
		executed.IsError = true
		executed.Output = "finish_turn summary is required"
		return executed
	}
	if req.Outcome == "blocked" && req.Blocker == "" {
		executed.IsError = true
		executed.Output = "finish_turn blocker is required when outcome is blocked"
		return executed
	}
	executed.TurnFinished = true
	executed.TurnOutcome = req.Outcome
	executed.FinishSummary = req.Summary
	executed.Output = nativeCompactJSON(map[string]any{
		"accepted": true,
		"outcome":  req.Outcome,
		"summary":  req.Summary,
		"blocker":  req.Blocker,
	})
	return executed
}
