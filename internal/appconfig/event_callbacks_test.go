package appconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/engine"
)

func TestEventCallbackSinkBuildsAppScopedRoutes(t *testing.T) {
	var gotAuth string
	var gotEvents []engine.NATSRuntimeEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event engine.NATSRuntimeEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Fatalf("decode callback: %v", err)
		}
		gotAuth = r.Header.Get("Authorization")
		gotEvents = append(gotEvents, event)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sink := EventCallbackSink(&Config{Apps: []App{
		{
			AppID: "usermaven",
			EventCallbacks: []EventCallback{{
				URL:        server.URL,
				Token:      "callback-token",
				EventTypes: []string{"run.completed"},
			}},
		},
		{AppID: "helpin"},
	}}, server.Client())

	sink.Emit(context.Background(), engine.Event{AppID: "usermaven", RunID: "run-1", Type: "run.started"})
	sink.Emit(context.Background(), engine.Event{AppID: "helpin", RunID: "run-2", Type: "run.completed"})
	sink.Emit(context.Background(), engine.Event{AppID: "usermaven", RunID: "run-1", Type: "run.completed"})

	if len(gotEvents) != 1 || gotEvents[0].AppID != "usermaven" || gotEvents[0].Type != "run.completed" {
		t.Fatalf("unexpected callback events: %#v", gotEvents)
	}
	if gotAuth != "Bearer callback-token" {
		t.Fatalf("unexpected callback auth %q", gotAuth)
	}
}
