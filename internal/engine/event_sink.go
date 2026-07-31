package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
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
	defaultNATSClientName            = "agent-runtime"
	defaultNATSDuplicateWindow       = 2 * time.Minute
	defaultNATSMaxAge                = 7 * 24 * time.Hour
	defaultNATSMaxBytes        int64 = 512 * 1024 * 1024
	defaultCallbackTimeout           = 10 * time.Second
	defaultCallbackAttempts          = 3
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

type CallbackEventSinkConfig struct {
	URL     string
	Token   string
	Timeout time.Duration
}

type CallbackEventSink struct {
	url    string
	token  string
	client *http.Client
	seq    atomic.Int64
}

// AppCallbackRoute scopes one HTTP callback to a single host application.
// An empty EventTypes list accepts every event emitted for the app.
type AppCallbackRoute struct {
	AppID      string
	URL        string
	Token      string
	EventTypes []string
}

type appCallbackRoute struct {
	sink       *CallbackEventSink
	eventTypes map[string]struct{}
}

// AppCallbackEventSink routes events only to callbacks configured for the
// event's app_id. It deliberately remains separate from the global event sink
// selected by AGENT_RUNTIME_EVENT_SINK so log/NATS topology stays shared while
// HTTP delivery and credentials remain app-scoped.
type AppCallbackEventSink struct {
	routes map[string][]appCallbackRoute
}

func NewNATSEventSink(publisher natsEventPublisher, subjectTemplate string) *NATSEventSink {
	subjectTemplate = strings.TrimSpace(subjectTemplate)
	if subjectTemplate == "" {
		subjectTemplate = sdk.DefaultNATSSubjectTemplate
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
	subject := sdk.RenderNATSSubject(s.subjectTemplate, envelope)
	if _, err := s.publisher.Publish(subject, payload, nats.MsgId(envelope.EventID)); err != nil {
		slog.Error("nats event sink: publish failed", "subject", subject, "app_id", event.AppID, "run_id", event.RunID, "type", event.Type, "error", err)
	}
}

func NewCallbackEventSink(url, token string, client *http.Client) *CallbackEventSink {
	if client == nil {
		client = &http.Client{Timeout: defaultCallbackTimeout}
	}
	return &CallbackEventSink{
		url:    strings.TrimSpace(url),
		token:  strings.TrimSpace(token),
		client: client,
	}
}

func NewAppCallbackEventSink(routes []AppCallbackRoute, client *http.Client) *AppCallbackEventSink {
	if client == nil {
		client = &http.Client{Timeout: defaultCallbackTimeout}
	}
	configured := make(map[string][]appCallbackRoute)
	for _, route := range routes {
		appID := strings.TrimSpace(route.AppID)
		url := strings.TrimSpace(route.URL)
		if appID == "" || url == "" {
			continue
		}
		eventTypes := make(map[string]struct{}, len(route.EventTypes))
		for _, eventType := range route.EventTypes {
			if eventType = strings.TrimSpace(eventType); eventType != "" {
				eventTypes[eventType] = struct{}{}
			}
		}
		configured[appID] = append(configured[appID], appCallbackRoute{
			sink:       NewCallbackEventSink(url, route.Token, client),
			eventTypes: eventTypes,
		})
	}
	return &AppCallbackEventSink{routes: configured}
}

func (s *AppCallbackEventSink) Emit(ctx context.Context, event Event) {
	if s == nil {
		return
	}
	routes := s.routes[strings.TrimSpace(event.AppID)]
	if len(routes) == 0 {
		return
	}
	eventType := strings.TrimSpace(event.Type)
	var wg sync.WaitGroup
	for _, route := range routes {
		if len(route.eventTypes) > 0 {
			if _, allowed := route.eventTypes[eventType]; !allowed {
				continue
			}
		}
		wg.Add(1)
		go func(route appCallbackRoute) {
			defer wg.Done()
			route.sink.Emit(ctx, event)
		}(route)
	}
	wg.Wait()
}

func (s *CallbackEventSink) Emit(ctx context.Context, event Event) {
	if s == nil || s.client == nil || strings.TrimSpace(s.url) == "" {
		return
	}
	envelope := runtimeEventEnvelope(event, s.seq.Add(1))
	payload, err := json.Marshal(envelope)
	if err != nil {
		slog.Error("callback event sink: marshal failed", "app_id", event.AppID, "run_id", event.RunID, "type", event.Type, "error", err)
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var lastStatus int
	var lastErr error
	for attempt := 1; attempt <= defaultCallbackAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(payload))
		if err != nil {
			slog.Error("callback event sink: request build failed", "url", s.url, "app_id", event.AppID, "run_id", event.RunID, "type", event.Type, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if s.token != "" {
			req.Header.Set("Authorization", "Bearer "+s.token)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			lastStatus = resp.StatusCode
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				slog.Error("callback event sink: post rejected", "url", s.url, "status", resp.StatusCode, "app_id", event.AppID, "run_id", event.RunID, "type", event.Type)
				return
			}
		}
		if attempt < defaultCallbackAttempts {
			timer := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
	if lastErr != nil {
		slog.Error("callback event sink: post failed", "url", s.url, "app_id", event.AppID, "run_id", event.RunID, "type", event.Type, "error", lastErr)
		return
	}
	if lastStatus != 0 {
		slog.Error("callback event sink: post rejected after retries", "url", s.url, "status", lastStatus, "app_id", event.AppID, "run_id", event.RunID, "type", event.Type)
	}
}

func runtimeEventEnvelope(event Event, sequenceNo int64) sdk.EventEnvelope {
	return sdk.EventEnvelope{
		EventID:    uuid.NewString(),
		SentAt:     time.Now().UTC(),
		SequenceNo: sequenceNo,
		AppID:      strings.TrimSpace(event.AppID),
		RunID:      strings.TrimSpace(event.RunID),
		HostRunID:  strings.TrimSpace(event.HostRunID),
		Type:       strings.TrimSpace(event.Type),
		Data:       event.Data,
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
		case "callback", "http", "webhook":
			sink, closeFn, err := openCallbackEventSinkFromEnv()
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

func openCallbackEventSinkFromEnv() (EventSink, func(), error) {
	cfg := CallbackEventSinkConfig{
		URL:   strings.TrimSpace(os.Getenv("AGENT_RUNTIME_EVENT_CALLBACK_URL")),
		Token: strings.TrimSpace(os.Getenv("AGENT_RUNTIME_EVENT_CALLBACK_TOKEN")),
	}
	if cfg.URL == "" {
		return nil, nil, fmt.Errorf("AGENT_RUNTIME_EVENT_CALLBACK_URL is required when AGENT_RUNTIME_EVENT_SINK includes callback")
	}
	return NewCallbackEventSink(cfg.URL, cfg.Token, &http.Client{Timeout: defaultCallbackTimeout}), func() {}, nil
}

func openNATSEventSinkFromEnv() (EventSink, func(), error) {
	cfg := NATSEventSinkConfig{
		URL:             strings.TrimSpace(os.Getenv("AGENT_RUNTIME_NATS_URL")),
		ClientName:      firstNonEmptyEnv("AGENT_RUNTIME_NATS_CLIENT_NAME", defaultNATSClientName),
		StreamName:      firstNonEmptyEnv("AGENT_RUNTIME_NATS_STREAM", defaultNATSStreamName),
		StreamSubjects:  splitCSV(firstNonEmptyEnv("AGENT_RUNTIME_NATS_STREAM_SUBJECTS", defaultNATSStreamSubject)),
		SubjectTemplate: firstNonEmptyEnv("AGENT_RUNTIME_NATS_SUBJECT_TEMPLATE", sdk.DefaultNATSSubjectTemplate),
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

// OpenV2EventPublisherFromEnv opens the versioned publisher when the shared
// event sink includes NATS. V1 publication remains independently configured.
func OpenV2EventPublisherFromEnv() (V2EventPublisher, func(), error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_RUNTIME_EVENT_SINK")))
	enabled := false
	for _, value := range splitCSV(mode) {
		if value == "nats" || value == "jetstream" {
			enabled = true
			break
		}
	}
	if !enabled {
		return nil, func() {}, nil
	}
	cfg := NATSEventSinkConfig{
		URL:            strings.TrimSpace(os.Getenv("AGENT_RUNTIME_NATS_URL")),
		ClientName:     firstNonEmptyEnv("AGENT_RUNTIME_NATS_CLIENT_NAME", defaultNATSClientName) + "-v2",
		StreamName:     firstNonEmptyEnv("AGENT_RUNTIME_NATS_STREAM", defaultNATSStreamName),
		StreamSubjects: splitCSV(firstNonEmptyEnv("AGENT_RUNTIME_NATS_STREAM_SUBJECTS", defaultNATSStreamSubject)),
		EnsureStream:   !envFalse("AGENT_RUNTIME_NATS_ENSURE_STREAM"),
	}
	if cfg.URL == "" {
		return nil, nil, fmt.Errorf("AGENT_RUNTIME_NATS_URL is required for v2 event publication")
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
	return &natsV2EventPublisher{publisher: js}, nc.Close, nil
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
