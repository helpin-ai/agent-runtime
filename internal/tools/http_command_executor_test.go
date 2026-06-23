package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPCommandExecutorPostsCommandRequest(t *testing.T) {
	var got CommandExecutionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/execute" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer command-token" {
			t.Fatalf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(CommandExecutionResponse{Output: json.RawMessage(`{"ok":true}`)})
	}))
	defer server.Close()

	output, err := HTTPCommandExecutor{BaseURL: server.URL, Token: "command-token"}.ExecuteCommand(context.Background(), CommandExecutionContext{
		AppID:      "host_app",
		RunID:      "run-1",
		TargetType: "task",
		TargetID:   "task-1",
	}, "pm.update_task_state", json.RawMessage(`{"state_id":"done"}`))
	if err != nil {
		t.Fatalf("execute command: %v", err)
	}
	if string(output) != `{"ok":true}` {
		t.Fatalf("unexpected output: %s", output)
	}
	if got.CommandName != "pm.update_task_state" || got.Meta.AppID != "host_app" || got.Meta.TargetID != "task-1" || string(got.Input) != `{"state_id":"done"}` {
		t.Fatalf("unexpected request: %#v input=%s", got, string(got.Input))
	}
}

func TestHTTPCommandExecutorReturnsRemoteError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CommandExecutionResponse{Error: "not allowed"})
	}))
	defer server.Close()

	_, err := HTTPCommandExecutor{BaseURL: server.URL}.ExecuteCommand(context.Background(), CommandExecutionContext{}, "pm.update_task_state", nil)
	if err == nil || err.Error() != "not allowed" {
		t.Fatalf("expected remote error, got %v", err)
	}
}
