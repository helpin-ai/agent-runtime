package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
)

type fakeNATSPublisher struct {
	subjects []string
	payloads [][]byte
}

func (p *fakeNATSPublisher) Publish(subject string, data []byte, _ ...nats.PubOpt) (*nats.PubAck, error) {
	p.subjects = append(p.subjects, subject)
	p.payloads = append(p.payloads, append([]byte(nil), data...))
	return &nats.PubAck{Stream: "AGENT_RUNTIME_EVENTS", Sequence: uint64(len(p.payloads))}, nil
}

func TestNATSEventSinkPublishesRuntimeEnvelope(t *testing.T) {
	publisher := &fakeNATSPublisher{}
	sink := NewNATSEventSink(publisher, "agent-runtime.events.{app_id}.{run_id}.{event_type}")

	sink.Emit(context.Background(), Event{
		AppID:     "app-a",
		RunID:     "run-1",
		HostRunID: "helpin-run-1",
		Type:      "assistant_message_delta",
		Data:      map[string]interface{}{"text": "hello"},
	})

	if len(publisher.subjects) != 1 {
		t.Fatalf("expected one published event, got %d", len(publisher.subjects))
	}
	if publisher.subjects[0] != "agent-runtime.events.app-a.run-1.assistant_message_delta" {
		t.Fatalf("unexpected subject %q", publisher.subjects[0])
	}
	var event NATSRuntimeEvent
	if err := json.Unmarshal(publisher.payloads[0], &event); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if event.EventID == "" || event.SentAt.IsZero() {
		t.Fatalf("expected event id and sent_at, got %#v", event)
	}
	if event.SequenceNo != 1 || event.AppID != "app-a" || event.RunID != "run-1" || event.Type != "assistant_message_delta" {
		t.Fatalf("unexpected event envelope: %#v", event)
	}
	if event.HostRunID != "helpin-run-1" {
		t.Fatalf("expected host run id in envelope, got %#v", event)
	}
	if event.Data["text"] != "hello" {
		t.Fatalf("unexpected event data: %#v", event.Data)
	}
}

func TestRenderNATSSubjectSanitizesTokens(t *testing.T) {
	subject := renderNATSSubject("events.{app_id}.{run_id}.{event_type}", NATSRuntimeEvent{
		AppID: "app a",
		RunID: "run/1",
		Type:  "tool*>call",
	})
	if subject != "events.app_a.run_1.tool__call" {
		t.Fatalf("unexpected subject %q", subject)
	}
}

func TestOpenEventSinkFromEnvDefaultsToLog(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_EVENT_SINK", "")
	sink, closeFn, err := OpenEventSinkFromEnv()
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	defer closeFn()
	if _, ok := sink.(SlogEventSink); !ok {
		t.Fatalf("expected default slog sink, got %T", sink)
	}
}

func TestOpenEventSinkFromEnvRequiresNATSURL(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_EVENT_SINK", "nats")
	t.Setenv("AGENT_RUNTIME_NATS_URL", "")
	if _, _, err := OpenEventSinkFromEnv(); err == nil {
		t.Fatalf("expected missing NATS URL error")
	}
}

func TestOpenEventSinkFromEnvSupportsNone(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_EVENT_SINK", "none")
	sink, closeFn, err := OpenEventSinkFromEnv()
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	defer closeFn()
	if _, ok := sink.(NoopEventSink); !ok {
		t.Fatalf("expected noop sink, got %T", sink)
	}
}
