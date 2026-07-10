package engine

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// PersistedEventSink writes the host-neutral event envelope to the shared run
// store. API and Temporal worker processes both install it, allowing run
// history and SSE replay across process boundaries.
type PersistedEventSink struct {
	Store agentcore.Store
}

func (s PersistedEventSink) Emit(ctx context.Context, event Event) {
	if s.Store == nil || event.AppID == "" || event.RunID == "" || IsTransientLiveEvent(event.Type) {
		return
	}
	persisted := &agentcore.AgentRunEvent{
		EventID:   uuid.NewString(),
		SentAt:    time.Now().UTC(),
		AppID:     event.AppID,
		RunID:     event.RunID,
		HostRunID: event.HostRunID,
		Type:      event.Type,
		Data:      event.Data,
	}
	if err := s.Store.AppendEvent(ctx, persisted); err != nil {
		slog.ErrorContext(ctx, "persist runtime event failed", "app_id", event.AppID, "run_id", event.RunID, "type", event.Type, "error", err)
	}
}

func IsTransientLiveEvent(eventType string) bool {
	switch eventType {
	case "assistant_message_delta", "reasoning_message_delta", "tool_call_args_delta", "activity_delta":
		return true
	default:
		return false
	}
}

var _ EventSink = PersistedEventSink{}
