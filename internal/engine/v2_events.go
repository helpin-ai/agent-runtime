package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const (
	EventSchemaVersionV2         = "2"
	defaultNATSV2SubjectTemplate = "agent-runtime.events.v2.{app_id}.{run_id}.{event_type}"
)

// V2EventEnvelope is the stable ordered wire contract consumed by v2 hosts.
type V2EventEnvelope struct {
	EventID       string                 `json:"event_id"`
	SentAt        time.Time              `json:"sent_at"`
	SequenceNo    int64                  `json:"sequence_no"`
	AppID         string                 `json:"app_id"`
	RunID         string                 `json:"run_id"`
	HostRunID     string                 `json:"host_run_id,omitempty"`
	SchemaVersion string                 `json:"schema_version"`
	TurnID        string                 `json:"turn_id,omitempty"`
	SegmentID     string                 `json:"segment_id,omitempty"`
	Revision      int64                  `json:"revision"`
	BaseRevision  int64                  `json:"base_revision,omitempty"`
	Type          string                 `json:"type"`
	Data          map[string]interface{} `json:"data,omitempty"`
}

// V2EventPublisher publishes a persisted event without changing its identity.
type V2EventPublisher interface {
	PublishV2(ctx context.Context, event agentcore.AgentRunEvent) error
}

type natsV2EventPublisher struct {
	publisher natsEventPublisher
}

func (p *natsV2EventPublisher) PublishV2(_ context.Context, event agentcore.AgentRunEvent) error {
	if p == nil || p.publisher == nil {
		return nil
	}
	envelope := V2EnvelopeFromPersisted(event)
	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal v2 event: %w", err)
	}
	subject := renderV2NATSSubject(envelope)
	_, err = p.publisher.Publish(subject, payload, nats.MsgId(envelope.EventID))
	return err
}

// V2EnvelopeFromPersisted maps the canonical durable row to the v2 wire shape.
func V2EnvelopeFromPersisted(event agentcore.AgentRunEvent) V2EventEnvelope {
	segmentID := firstEventDataString(event.Data, "segment_id", "message_id", "tool_call_id", "activity_id")
	turnID := firstEventDataString(event.Data, "turn_id", "parent_message_id")
	if turnID == "" && strings.Contains(event.Type, "assistant_message") {
		turnID = segmentID
	}
	return V2EventEnvelope{
		EventID: event.EventID, SentAt: event.SentAt, SequenceNo: event.SequenceNo,
		AppID: event.AppID, RunID: event.RunID, HostRunID: event.HostRunID,
		SchemaVersion: EventSchemaVersionV2, TurnID: turnID, SegmentID: segmentID,
		Revision: event.SequenceNo, Type: event.Type, Data: event.Data,
	}
}

func renderV2NATSSubject(event V2EventEnvelope) string {
	replacer := strings.NewReplacer(
		"{app_id}", natsToken(event.AppID, true),
		"{run_id}", natsToken(event.RunID, false),
		"{event_type}", natsToken(event.Type, false),
	)
	return replacer.Replace(defaultNATSV2SubjectTemplate)
}

func natsToken(value string, replaceDots bool) string {
	value = strings.TrimSpace(value)
	value = strings.NewReplacer(" ", "_", "\t", "_", "\n", "_", "\r", "_", "/", "_", "\\", "_", "*", "_", ">", "_").Replace(value)
	if replaceDots {
		value = strings.ReplaceAll(value, ".", "_")
	}
	if value == "" {
		return "_"
	}
	return value
}

func firstEventDataString(data map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
