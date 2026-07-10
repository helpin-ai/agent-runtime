package engine

import (
	"context"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestPersistedEventSinkStoresTimelineEventsButNotTokenDeltas(t *testing.T) {
	ctx := context.Background()
	memory := store.NewMemory()
	sink := PersistedEventSink{Store: memory}
	sink.Emit(ctx, Event{AppID: "app-a", RunID: "run-1", Type: "run.started"})
	sink.Emit(ctx, Event{AppID: "app-a", RunID: "run-1", Type: "assistant_message_delta", Data: map[string]interface{}{"text": "token"}})
	sink.Emit(ctx, Event{AppID: "app-a", RunID: "run-1", Type: "assistant_message_completed", Data: map[string]interface{}{"text": "done"}})

	events, err := memory.ListEvents(ctx, "app-a", "run-1")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 || events[0].Type != "run.started" || events[1].Type != "assistant_message_completed" {
		t.Fatalf("unexpected persisted events: %#v", events)
	}
}
