package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestStrictModelAdmissionRejectsBeforeQueueing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      *sdk.RunModel
		credential *sdk.ModelCredential
	}{
		{"neither", nil, nil},
		{"model only", &sdk.RunModel{Provider: "openai", Model: "test"}, nil},
		{"credential only", nil, &sdk.ModelCredential{Type: "api_key", APIKey: "private"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := store.NewMemory()
			agent := testAgent("helpin")
			if err := db.CreateAgent(context.Background(), &agent); err != nil {
				t.Fatal(err)
			}
			e := New(Config{Store: db, RequireRunModelCredentials: func(app string) bool { return app == "helpin" }})
			_, err := e.StartRun(context.Background(), StartRunRequest{AppID: "helpin", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "ticket", ID: "target"}, Model: tc.model, ModelCredential: tc.credential})
			if !errors.Is(err, ErrRunModelCredentialsRequired) {
				t.Fatalf("admission error=%v", err)
			}
			runs, err := db.ListRunsByStatus(context.Background(), agentcore.RunStatusQueued)
			if err != nil || len(runs) != 0 {
				t.Fatalf("rejection queued a run: %v %v", runs, err)
			}
		})
	}
}

func TestStrictModelPolicyCannotBeDisabledInJSON(t *testing.T) {
	var request StartRunRequest
	if err := json.Unmarshal([]byte(`{"app_id":"helpin","agent_id":"agent","target":{"type":"ticket","id":"target"},"require_run_model_credentials":false}`), &request); err != nil {
		t.Fatal(err)
	}
	db := store.NewMemory()
	agent := testAgent("helpin")
	agent.ID = "agent"
	if err := db.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	e := New(Config{Store: db, RequireRunModelCredentials: func(string) bool { return true }})
	if _, err := e.StartRun(context.Background(), request); !errors.Is(err, ErrRunModelCredentialsRequired) {
		t.Fatalf("caller weakened policy: %v", err)
	}
}

func TestStrictAdmissionUsesPersistedCredentialOnRetry(t *testing.T) {
	db := store.NewMemory()
	agent := testAgent("helpin")
	if err := db.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	e := New(Config{Store: db, RequireRunModelCredentials: func(string) bool { return true }, DefaultExecutionMode: ExecutionModeDurable,
		Durable: &recordingDurableExecutor{}, ModelCredentials: &modelauth.Manager{Store: db, Key: []byte(strings.Repeat("k", 32))}, Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider()})
	request := StartRunRequest{AppID: "helpin", HostRunID: "host-run", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "ticket", ID: "target"}, Model: &sdk.RunModel{Provider: "openai", Model: "test"}, ModelCredential: &sdk.ModelCredential{Type: "api_key", APIKey: "private"}}
	first, err := e.StartRun(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Model = nil
	request.ModelCredential = nil
	second, err := e.StartRun(context.Background(), request)
	if err != nil || second.ID != first.ID || second.Input.CredentialSource != "app" {
		t.Fatalf("retry=%+v err=%v", second, err)
	}
}

func TestDefaultCredentialAppRemainsCompatible(t *testing.T) {
	db := store.NewMemory()
	agent := testAgent("usermaven")
	if err := db.CreateAgent(context.Background(), &agent); err != nil {
		t.Fatal(err)
	}
	e := New(Config{Store: db, RequireRunModelCredentials: func(app string) bool { return app == "helpin" }, DefaultExecutionMode: ExecutionModeDurable,
		Durable: &recordingDurableExecutor{}, Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider()})
	run, err := e.StartRun(context.Background(), StartRunRequest{AppID: "usermaven", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "ticket", ID: "target"}})
	if err != nil || run.Input.CredentialSource == "app" {
		t.Fatalf("default app=%+v err=%v", run, err)
	}
	// Enabling policy later must also cover repair and worker execution paths.
	e.cfg.RequireRunModelCredentials = func(string) bool { return true }
	if err := e.executionPolicy(&agent, run); !errors.Is(err, ErrRunModelCredentialsRequired) {
		t.Fatalf("execution bypass: %v", err)
	}
	if err := e.admitExistingRun(context.Background(), run); !errors.Is(err, ErrRunModelCredentialsRequired) {
		t.Fatalf("resume bypass: %v", err)
	}
	if n, err := e.ReconcileDurableRuns(context.Background(), time.Time{}); n != 0 || !errors.Is(err, ErrRunModelCredentialsRequired) {
		t.Fatalf("repair bypass: %d %v", n, err)
	}
}
