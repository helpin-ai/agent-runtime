package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/helpin-ai/agent-runtime-go"
)

const (
	defaultEventSinkMode             = "log"
	defaultNATSStreamName            = "AGENT_RUNTIME_EVENTS"
	defaultNATSStreamSubject         = "agent-runtime.events.>"
	defaultNATSSubjectTemplate       = "agent-runtime.events.{app_id}.{run_id}.{event_type}"
	defaultNATSClientName            = "agent-runtime"
	defaultNATSDuplicateWindow       = 2 * time.Minute
	defaultNATSMaxAge                = 7 * 24 * time.Hour
	defaultNATSMaxBytes        int64 = 512 * 1024 * 1024
)

type NoopEventSink struct{}

func (NoopEventSink) Emit(context.Context, Event) {}

type MultiEventSink []EventSink

func (s MultiEventSink) Emit(ctx context.Context, event Event) {
	for _, sink := range s {
		if sink != nil {
			sink.Emit(ctx, event)
		}
	}
}

type NATSEventSinkConfig struct {
	URL             string
	ClientName      string
	StreamName      string
	StreamSubjects  []string
	SubjectTemplate string
	EnsureStream    bool
}

type NATSEventSink struct {
	publisher       natsEventPublisher
	subjectTemplate string
	seq             atomic.Int64
}

type natsEventPublisher interface {
	Publish(subject string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error)
}

type NATSRuntimeEvent = sdk.EventEnvelope

func NewNATSEventSink(publisher natsEventPublisher, subjectTemplate string) *NATSEventSink {
	subjectTemplate = strings.TrimSpace(subjectTemplate)
	if subjectTemplate == "" {
		subjectTemplate = defaultNATSSubjectTemplate
	}
	return &NATSEventSink{publisher: publisher, subjectTemplate: subjectTemplate}
}

func (s *NATSEventSink) Emit(_ context.Context, event Event) {
	if s == nil || s.publisher == nil {
		return
	}
	envelope := NATSRuntimeEvent{
		EventID:    uuid.NewString(),
		SentAt:     time.Now().UTC(),
		SequenceNo: s.seq.Add(1),
		AppID:      strings.TrimSpace(event.AppID),
		RunID:      strings.TrimSpace(event.RunID),
		HostRunID:  strings.TrimSpace(event.HostRunID),
		Type:       strings.TrimSpace(event.Type),
		Data:       event.Data,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		slog.Error("nats event sink: marshal failed", "app_id", event.AppID, "run_id", event.RunID, "type", event.Type, "error", err)
		return
	}
	subject := renderNATSSubject(s.subjectTemplate, envelope)
	if _, err := s.publisher.Publish(subject, payload, nats.MsgId(envelope.EventID)); err != nil {
		slog.Error("nats event sink: publish failed", "subject", subject, "app_id", event.AppID, "run_id", event.RunID, "type", event.Type, "error", err)
	}
}

func OpenEventSinkFromEnv() (EventSink, func(), error) {
	mode := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_EVENT_SINK"))
	if mode == "" {
		mode = defaultEventSinkMode
	}
	parts := splitCSV(mode)
	if len(parts) == 0 {
		parts = []string{defaultEventSinkMode}
	}
	sinks := make([]EventSink, 0, len(parts))
	closers := make([]func(), 0, 1)
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "", "none", "noop":
			if len(parts) == 1 {
				return NoopEventSink{}, func() {}, nil
			}
		case "log", "slog":
			sinks = append(sinks, SlogEventSink{})
		case "nats", "jetstream":
			sink, closeFn, err := openNATSEventSinkFromEnv()
			if err != nil {
				for _, closer := range closers {
					closer()
				}
				return nil, nil, err
			}
			sinks = append(sinks, sink)
			closers = append(closers, closeFn)
		default:
			for _, closer := range closers {
				closer()
			}
			return nil, nil, fmt.Errorf("unsupported AGENT_RUNTIME_EVENT_SINK value %q", part)
		}
	}
	if len(sinks) == 0 {
		return NoopEventSink{}, func() {}, nil
	}
	closeAll := func() {
		for _, closer := range closers {
			closer()
		}
	}
	if len(sinks) == 1 {
		return sinks[0], closeAll, nil
	}
	return MultiEventSink(sinks), closeAll, nil
}

func openNATSEventSinkFromEnv() (EventSink, func(), error) {
	cfg := NATSEventSinkConfig{
		URL:             strings.TrimSpace(os.Getenv("AGENT_RUNTIME_NATS_URL")),
		ClientName:      firstNonEmptyEnv("AGENT_RUNTIME_NATS_CLIENT_NAME", defaultNATSClientName),
		StreamName:      firstNonEmptyEnv("AGENT_RUNTIME_NATS_STREAM", defaultNATSStreamName),
		StreamSubjects:  splitCSV(firstNonEmptyEnv("AGENT_RUNTIME_NATS_STREAM_SUBJECTS", defaultNATSStreamSubject)),
		SubjectTemplate: firstNonEmptyEnv("AGENT_RUNTIME_NATS_SUBJECT_TEMPLATE", defaultNATSSubjectTemplate),
		EnsureStream:    !envFalse("AGENT_RUNTIME_NATS_ENSURE_STREAM"),
	}
	if cfg.URL == "" {
		return nil, nil, fmt.Errorf("AGENT_RUNTIME_NATS_URL is required when AGENT_RUNTIME_EVENT_SINK includes nats")
	}
	nc, err := nats.Connect(cfg.URL,
		nats.Name(cfg.ClientName),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, nil, err
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	if cfg.EnsureStream {
		if err := ensureNATSStream(js, cfg); err != nil {
			nc.Close()
			return nil, nil, err
		}
	}
	return NewNATSEventSink(js, cfg.SubjectTemplate), nc.Close, nil
}

func ensureNATSStream(js nats.JetStreamContext, cfg NATSEventSinkConfig) error {
	if js == nil {
		return fmt.Errorf("nats jetstream context is nil")
	}
	stream := strings.TrimSpace(cfg.StreamName)
	if stream == "" {
		stream = defaultNATSStreamName
	}
	subjects := cfg.StreamSubjects
	if len(subjects) == 0 {
		subjects = []string{defaultNATSStreamSubject}
	}
	config := &nats.StreamConfig{
		Name:       stream,
		Subjects:   subjects,
		Storage:    nats.FileStorage,
		Retention:  nats.LimitsPolicy,
		Discard:    nats.DiscardOld,
		Duplicates: defaultNATSDuplicateWindow,
		MaxAge:     defaultNATSMaxAge,
		MaxBytes:   defaultNATSMaxBytes,
	}
	if _, err := js.StreamInfo(stream); err != nil {
		if !errors.Is(err, nats.ErrStreamNotFound) {
			return err
		}
		_, err = js.AddStream(config)
		return err
	}
	_, err := js.UpdateStream(config)
	return err
}

func renderNATSSubject(template string, event NATSRuntimeEvent) string {
	replacer := strings.NewReplacer(
		"{app_id}", natsSubjectToken(event.AppID),
		"{run_id}", natsSubjectToken(event.RunID),
		"{event_type}", natsSubjectToken(event.Type),
		"{type}", natsSubjectToken(event.Type),
	)
	return replacer.Replace(template)
}

func natsSubjectToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "_"
	}
	value = strings.NewReplacer(" ", "_", "\t", "_", "\n", "_", "\r", "_", "/", "_", "\\", "_", "*", "_", ">", "_").Replace(value)
	return value
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func firstNonEmptyEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

func envFalse(key string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return value == "0" || value == "false" || value == "no" || value == "off"
}
