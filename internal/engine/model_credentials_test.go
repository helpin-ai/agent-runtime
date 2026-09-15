package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type modelAuthenticationTestAdapter struct{}

func (modelAuthenticationTestAdapter) Kind() string { return agentcore.RuntimeNativeSDK }
func (modelAuthenticationTestAdapter) Execute(*runtime.ExecutionContext) (*runtime.Result, error) {
	return nil, &modelauth.AuthenticationError{Provider: "openai", ConnectionID: "personal", Reason: "unauthorized"}
}

func TestRunCredentialAdmissionPauseAndTerminalCleanup(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	agent := testAgent("app")
	agent.Provider = "openai"
	agent.Model = "test"
	if err := db.CreateAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	manager := &modelauth.Manager{Store: db, Key: []byte(strings.Repeat("k", 32))}
	e := New(Config{Store: db, DefaultExecutionMode: ExecutionModeDurable, Durable: &recordingDurableExecutor{}, ModelCredentials: manager, Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider(), Runtimes: runtime.NewRegistry(modelAuthenticationTestAdapter{})})
	run, err := e.StartRun(ctx, StartRunRequest{AppID: "app", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "ticket", ID: "ticket"}, ModelCredential: &sdk.ModelCredential{Type: "api_key", APIKey: "private-secret", ConnectionID: "personal"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(run)
	if strings.Contains(string(raw), "private-secret") || run.Input.CredentialSource != "app" || run.Input.Model.Provider != "openai" {
		t.Fatal("invalid public run contract")
	}
	result, err := e.ExecuteRunOnce(ctx, "app", run.ID)
	if err != nil || !result.AwaitingAuth {
		t.Fatalf("auth did not pause: %v", err)
	}
	paused, err := db.GetRun(ctx, "app", run.ID)
	if err != nil || paused.Status != agentcore.RunStatusPaused || paused.PauseReason != agentcore.PauseReasonAuth {
		t.Fatal("wrong pause status")
	}
	record, err := db.GetRunModelCredential(ctx, "app", run.ID)
	if err != nil || record == nil || len(record.EncryptedCredential) == 0 {
		t.Fatal("paused credential lost")
	}
	if _, err = e.UpdateRunModelCredential(ctx, "other", run.ID, sdk.ModelCredential{Type: "api_key", APIKey: "rotated", ConnectionID: "personal"}); err == nil {
		t.Fatal("cross-app rotation accepted")
	}
	if _, err = e.UpdateRunModelCredential(ctx, "app", run.ID, sdk.ModelCredential{Type: "api_key", APIKey: "rotated", ConnectionID: "personal"}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.CancelRun(ctx, "app", run.ID); err != nil {
		t.Fatal(err)
	}
	record, err = db.GetRunModelCredential(ctx, "app", run.ID)
	if err != nil || !record.Revoked || len(record.EncryptedCredential) != 0 {
		t.Fatal("terminal secret retained")
	}
	if _, err = e.UpdateRunModelCredential(ctx, "app", run.ID, sdk.ModelCredential{Type: "api_key", APIKey: "another", ConnectionID: "personal"}); err == nil {
		t.Fatal("terminal credential resurrected")
	}
}
