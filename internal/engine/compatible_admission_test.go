package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestCompatibleAdmissionRequiresTrustedBindingAndCredentialMode(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	agent := testAgent("helpin")
	if err := db.CreateAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	e := New(Config{Store: db, DefaultExecutionMode: ExecutionModeDurable, Durable: &recordingDurableExecutor{}, ModelCredentials: &modelauth.Manager{Store: db, Key: []byte(strings.Repeat("k", 32))}, Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider()})
	request := StartRunRequest{AppID: "helpin", HostRunID: "host", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "ticket", ID: "target"}, Model: &sdk.RunModel{Provider: "openai_compatible", Model: "local", Endpoint: &sdk.ModelEndpoint{ID: "local", BaseURL: "http://127.0.0.1:8081/v1", AuthMode: "none"}}, ModelCredential: &sdk.ModelCredential{Type: "none"}}
	if _, err := e.StartRun(ctx, request); err == nil {
		t.Fatal("missing app approval accepted")
	}
	e.cfg.ValidateRunModelEndpoint = func(string, *sdk.RunModel) error { return nil }
	request.ModelCredential = nil
	if _, err := e.StartRun(ctx, request); err == nil {
		t.Fatal("omitted no-auth specification accepted")
	}
	request.ModelCredential = &sdk.ModelCredential{Type: "api_key", APIKey: "key"}
	if _, err := e.StartRun(ctx, request); err == nil {
		t.Fatal("wrong auth mode accepted")
	}
	runs, err := db.ListRunsByStatus(ctx, agentcore.RunStatusQueued)
	if err != nil || len(runs) != 0 {
		t.Fatalf("rejection persisted a queued run: %v", err)
	}
	request.ModelCredential = &sdk.ModelCredential{Type: "none"}
	run, err := e.StartRun(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if run.Input.CredentialSource != "app" {
		t.Fatal("explicit no-auth used runtime defaults")
	}
	request.Model, request.ModelCredential = nil, nil
	retry, err := e.StartRun(ctx, request)
	if err != nil || retry.ID != run.ID {
		t.Fatalf("idempotent retry lost binding: %v", err)
	}
	e.cfg.ValidateRunModelEndpoint = func(string, *sdk.RunModel) error { return errors.New("approval removed") }
	if err := e.admitExistingRun(ctx, run); err == nil {
		t.Fatal("recovery ignored removed endpoint approval")
	}
}
