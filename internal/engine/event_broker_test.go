package engine

import (
	"context"
	"testing"
)

func TestEventBrokerDeliversToRunSubscribers(t *testing.T) {
	b := NewEventBroker()
	ch, unsub := b.Subscribe("run-1")
	defer unsub()

	// Event for a different run must not be delivered here.
	b.Emit(context.Background(), Event{RunID: "run-2", Type: "run.started"})
	b.Emit(context.Background(), Event{RunID: "run-1", Type: "assistant_message_delta"})

	got := <-ch
	if got.Type != "assistant_message_delta" || got.RunID != "run-1" {
		t.Fatalf("unexpected event: %+v", got)
	}
	select {
	case extra := <-ch:
		t.Fatalf("did not expect another event, got %+v", extra)
	default:
	}
}

func TestEventBrokerUnsubscribeStopsDelivery(t *testing.T) {
	b := NewEventBroker()
	ch, unsub := b.Subscribe("run-1")
	unsub()
	if _, open := <-ch; open {
		t.Fatal("channel should be closed after unsubscribe")
	}
	// Emitting after unsubscribe must not panic.
	b.Emit(context.Background(), Event{RunID: "run-1", Type: "run.completed"})
}

func TestEventBrokerDoesNotBlockOnSlowSubscriber(t *testing.T) {
	b := NewEventBroker()
	_, unsub := b.Subscribe("run-1")
	defer unsub()
	// Far more than the buffer (64); must not block (excess dropped).
	for i := 0; i < 500; i++ {
		b.Emit(context.Background(), Event{RunID: "run-1", Type: "tool_call_args_delta"})
	}
}
