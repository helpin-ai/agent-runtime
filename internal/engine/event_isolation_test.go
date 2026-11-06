package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

type recordingEventSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *recordingEventSink) Emit(_ context.Context, event Event) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *recordingEventSink) runIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events))
	for _, event := range s.events {
		out = append(out, event.RunID)
	}
	return out
}

// Hosts resolve every event against runs they started; Helpin NAKs the rest.
// Runtime-created runs must therefore never reach the global sink or app
// callbacks, even for callbacks subscribed to every event type, while their
// events stay in run history.
func TestHostDeliveryWithholdsRuntimeCreatedRuns(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	hostGroup, err := mem.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a"})
	if err != nil {
		t.Fatal(err)
	}
	jobGroup, err := mem.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a", OwnerKind: agentcore.RunGroupOwnerWorkJob})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []struct{ id, group string }{{"ordinary", ""}, {"host-member", hostGroup.ID}, {"job-worker", jobGroup.ID}} {
		if err := mem.CreateRun(agentcore.WithRunGroupMembership(ctx, agentcore.RunGroupMembership{GroupID: run.group}), &agentcore.AgentRun{ID: run.id, AppID: "app-a", AgentID: "agent"}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var callbacks []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event struct {
			RunID string `json:"run_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&event)
		mu.Lock()
		callbacks = append(callbacks, event.RunID)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	hidden := &HiddenRuns{Store: mem}
	global := &recordingEventSink{}
	sinks := MultiEventSink{
		PersistedEventSink{Store: mem, Hidden: hidden},
		HostDeliverySink{Sink: global, Hidden: hidden},
		HostDeliverySink{Sink: NewAppCallbackEventSink([]AppCallbackRoute{{AppID: "app-a", URL: server.URL}}, server.Client()), Hidden: hidden},
	}
	for _, runID := range []string{"ordinary", "host-member", "job-worker", "job-worker"} {
		sinks.Emit(ctx, Event{AppID: "app-a", RunID: runID, Type: "run.completed"})
	}

	if got := global.runIDs(); len(got) != 2 || got[0] != "ordinary" || got[1] != "host-member" {
		t.Fatalf("global sink received %v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(callbacks)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	got := append([]string(nil), callbacks...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("app callbacks received %v", got)
	}
	for _, runID := range got {
		if runID == "job-worker" {
			t.Fatalf("job worker event reached app callbacks: %v", got)
		}
	}
	events, err := mem.ListEvents(ctx, "app-a", "job-worker")
	if err != nil || len(events) != 2 {
		t.Fatalf("job worker history = %d events, %v", len(events), err)
	}
}
