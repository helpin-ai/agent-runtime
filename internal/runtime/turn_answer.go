package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// turnAnswer is separate from provider prose: a finish call can accompany a
// preamble, and provider message persistence must never hide its actual answer.
// Native checkpoints retain this value so recovery republishes the same ID.
type turnAnswer struct {
	MessageID string
	Content   string
}

const turnAnswerMessageType = "assistant_final"

func newTurnAnswer(execCtx *ExecutionContext, callID, content string) *turnAnswer {
	identity := strings.Join([]string{execCtx.Run.AppID, execCtx.Run.ID, nativeResumeKey(execCtx), callID}, "\x00")
	return &turnAnswer{
		MessageID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("turn-answer\x00"+identity)).String(),
		Content:   content,
	}
}

// publishTurnAnswer stores the canonical message before making it visible and
// before the engine can settle the turn. AppendMessage deduplicates by runtime
// message ID; event replay uses that same ID in every host projection.
func publishTurnAnswer(ctx context.Context, execCtx *ExecutionContext, answer *turnAnswer) error {
	if answer == nil || answer.MessageID == "" || strings.TrimSpace(answer.Content) == "" {
		return fmt.Errorf("turn answer is required")
	}
	if execCtx == nil || execCtx.Run == nil || execCtx.Store == nil {
		return fmt.Errorf("turn answer requires runtime storage")
	}
	message := &agentcore.AgentRunMessage{
		AppID:            execCtx.Run.AppID,
		RunID:            execCtx.Run.ID,
		RuntimeMessageID: answer.MessageID,
		Role:             "assistant",
		Content:          answer.Content,
		MessageType:      turnAnswerMessageType,
		ContentBlocks:    marshalNativeBlocks([]NativeBlock{{Type: nativeBlockTypeText, Text: answer.Content}}),
	}
	if err := execCtx.Store.AppendMessage(ctx, message); err != nil {
		return fmt.Errorf("persist turn answer: %w", err)
	}
	// Use the stored row on replay, including its original identity/content.
	answer.MessageID, answer.Content = message.RuntimeMessageID, message.Content
	emitNativeEvent(ctx, execCtx, "assistant_message_completed", map[string]any{
		"message_id":   message.RuntimeMessageID,
		"text":         message.Content,
		"content":      message.Content,
		"message_type": turnAnswerMessageType,
	})
	return nil
}

func recoverNativeTurnAnswer(execCtx *ExecutionContext, result *nativeExecutionResult) (*turnAnswer, error) {
	if assistant := nativeLastAssistantMessage(result.Messages); assistant != nil {
		calls := nativeToolCallBlocks(*assistant)
		if len(calls) == 1 && calls[0].ToolName == nativeToolFinishTurn {
			finished := executeNativeFinishTurn(calls[0])
			if finished.TurnFinished {
				return newTurnAnswer(execCtx, finished.ToolCallID, finished.FinishSummary), nil
			}
		}
	}
	return nil, fmt.Errorf("finished native checkpoint is missing its accepted answer")
}
