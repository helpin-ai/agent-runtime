package engine

import (
	"context"
	"sync"
)

// EventBroker is an in-process fan-out EventSink. It receives every emitted
// Event (engine lifecycle events plus the runtime adapters' token-level
// assistant_message_* / tool_call_* events) and delivers them to per-run
// subscribers, so an SSE endpoint can stream a run live.
//
// Sends are non-blocking: if a subscriber's buffer is full (a slow client) the
// event is dropped for that subscriber rather than stalling the engine. The
// client reconciles against the persisted run via a refetch.
type EventBroker struct {
	mu     sync.Mutex
	nextID int
	subs   map[string]map[int]chan Event
}

func NewEventBroker() *EventBroker {
	return &EventBroker{subs: map[string]map[int]chan Event{}}
}

// Emit fans an event out to every subscriber of event.RunID.
func (b *EventBroker) Emit(_ context.Context, event Event) {
	if b == nil || event.RunID == "" {
		return
	}
	b.mu.Lock()
	chans := make([]chan Event, 0, len(b.subs[event.RunID]))
	for _, ch := range b.subs[event.RunID] {
		chans = append(chans, ch)
	}
	b.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- event:
		default: // slow subscriber — drop rather than block the engine
		}
	}
}

// Subscribe returns a channel of events for a run and an unsubscribe function
// the caller must invoke when done.
func (b *EventBroker) Subscribe(runID string) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	if b.subs[runID] == nil {
		b.subs[runID] = map[int]chan Event{}
	}
	id := b.nextID
	b.nextID++
	b.subs[runID][id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if subs := b.subs[runID]; subs != nil {
			delete(subs, id)
			if len(subs) == 0 {
				delete(b.subs, runID)
			}
		}
		b.mu.Unlock()
		close(ch)
	}
}
