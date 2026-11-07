package api

import (
	"context"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSteeringAPIRequiresServiceAuthTenantAndRevision(t *testing.T) {
	db := store.NewMemory()
	ctx := context.Background()
	if err := db.CreateAgent(ctx, &agentcore.Agent{ID: "agent", AppID: "app", RuntimeKind: agentcore.RuntimeNativeSDK}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, &agentcore.AgentRun{ID: "run", AppID: "app", AgentID: "agent", Status: agentcore.RunStatusPaused, RuntimeKind: agentcore.RuntimeNativeSDK}); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(Config{Store: db, Engine: engine.New(engine.Config{Store: db}), ServiceToken: "service"})
	body := `{"command_id":"c","expected_revision":0,"external_actor_id":"actor","content":"Research only"}`
	for _, test := range []struct {
		name, app, token, body string
		status                 int
	}{
		{"unauthenticated", "app", "", body, http.StatusUnauthorized},
		{"foreign tenant", "foreign", "service", body, http.StatusBadRequest},
		{"missing actor", "app", "service", `{"command_id":"c","content":"Research"}`, http.StatusBadRequest},
		{"unknown field", "app", "service", `{"command_id":"c","external_actor_id":"actor","content":"Research","grant_tools":["write"]}`, http.StatusBadRequest},
		{"accepted", "app", "service", body, http.StatusAccepted},
		{"idempotent retry", "app", "service", body, http.StatusAccepted},
		{"conflicting retry", "app", "service", strings.Replace(body, "Research only", "Changed", 1), http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/runs/run/steering?app_id="+test.app, strings.NewReader(test.body))
			if test.token != "" {
				request.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	run, err := db.GetRun(ctx, "app", "run")
	if err != nil || run.Status != agentcore.RunStatusPaused {
		t.Fatal("steering resumed run")
	}
}
