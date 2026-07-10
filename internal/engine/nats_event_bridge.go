package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	sdk "github.com/helpin-ai/agent-runtime-go"
)

// OpenNATSEventBridgeFromEnv forwards the shared worker event subject into an
// API-local broker. It is intentionally live-only; durable history is supplied
// by PersistedEventSink.
func OpenNATSEventBridgeFromEnv(sink EventSink) (bool, func(), error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_RUNTIME_EVENT_SINK")))
	enabled := false
	for _, value := range splitCSV(mode) {
		if value == "nats" || value == "jetstream" {
			enabled = true
			break
		}
	}
	if !enabled {
		return false, func() {}, nil
	}
	url := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_NATS_URL"))
	if url == "" {
		return false, nil, fmt.Errorf("AGENT_RUNTIME_NATS_URL is required for the NATS event bridge")
	}
	conn, err := nats.Connect(url, nats.Name("agent-runtime-api-event-bridge"))
	if err != nil {
		return false, nil, err
	}
	subject := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_NATS_SUBSCRIBE_SUBJECT"))
	if subject == "" {
		subject = defaultNATSStreamSubject
	}
	subscription, err := conn.Subscribe(subject, func(message *nats.Msg) {
		var envelope sdk.EventEnvelope
		if json.Unmarshal(message.Data, &envelope) != nil {
			return
		}
		sink.Emit(nil, Event{
			AppID: envelope.AppID, RunID: envelope.RunID, HostRunID: envelope.HostRunID,
			Type: envelope.Type, Data: envelope.Data,
		})
	})
	if err != nil {
		conn.Close()
		return false, nil, err
	}
	if err := conn.FlushTimeout(5 * time.Second); err != nil {
		_ = subscription.Unsubscribe()
		conn.Close()
		return false, nil, err
	}
	return true, func() {
		_ = subscription.Unsubscribe()
		conn.Close()
	}, nil
}
