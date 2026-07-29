package engine

import (
	"context"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestPersistedEventSinkStoresTimelineEventsButNotTokenDeltasForV1(t *testing.T) {
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

type captureV2Publisher struct {
	events []agentcore.AgentRunEvent
}

func (p *captureV2Publisher) PublishV2(_ context.Context, event agentcore.AgentRunEvent) error {
	p.events = append(p.events, event)
	return nil
}

func TestPersistedEventSinkStoresAndPublishesTokenDeltasForV2(t *testing.T) {
	memory := store.NewMemory()
	run := &agentcore.AgentRun{ID: "run-1", AppID: "helpin"}
	if err := memory.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	publisher := &captureV2Publisher{}
	sink := PersistedEventSink{
		Store:       memory,
		V2Enabled:   func(appID string) bool { return appID == "helpin" },
		V2Publisher: publisher,
	}
	sink.Emit(context.Background(), Event{AppID: "helpin", RunID: run.ID, Type: "assistant_message_delta", Data: map[string]interface{}{"message_id": "message-1", "content": " hello"}})

	events, err := memory.ListEvents(context.Background(), "helpin", run.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].SequenceNo != 1 || len(publisher.events) != 1 || publisher.events[0].EventID != events[0].EventID {
		t.Fatalf("unexpected durable v2 events: stored=%#v published=%#v", events, publisher.events)
	}
}
