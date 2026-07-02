package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientStartRunDefaultsAppIDAndAuth(t *testing.T) {
	var gotAuth string
	var got StartRunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AgentRun{ID: "run-1", AppID: got.AppID, HostRunID: got.HostRunID})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, WithAppID("helpin"), WithServiceToken("token-1"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	run, err := client.StartRun(context.Background(), StartRunRequest{
		HostRunID: "host-1",
		AgentID:   "agent-1",
		Target:    TargetRef{Type: "task", ID: "task-1"},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if got.AppID != "helpin" || got.HostRunID != "host-1" {
		t.Fatalf("unexpected start request: %#v", got)
	}
	if gotAuth != "Bearer token-1" {
		t.Fatalf("authorization header = %q", gotAuth)
	}
	if run.ID != "run-1" || run.AppID != "helpin" {
		t.Fatalf("unexpected run: %#v", run)
	}
}

func TestClientListMessagesUsesAppQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs/run-1/messages" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("app_id"); got != "helpin" {
			t.Fatalf("app_id query = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]AgentRunMessage{{ID: "msg-1", Role: "assistant"}})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, WithAppID("helpin"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	messages, err := client.ListMessages(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(messages) != 1 || messages[0].ID != "msg-1" {
		t.Fatalf("unexpected messages: %#v", messages)
	}
}
