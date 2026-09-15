package runtime

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Opt-in paid smoke: verifies the run credential path with no global API key.
func TestNativeRunCredentialLiveSmoke(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_RUN_CREDENTIAL_SMOKE") != "1" {
		t.Skip("opt-in live provider test")
	}
	key := os.Getenv("RUN_CREDENTIAL_SMOKE_KEY")
	if key == "" {
		t.Fatal("RUN_CREDENTIAL_SMOKE_KEY required")
	}
	t.Setenv("OPENAI_API_KEY", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	x := contextTestExec(t)
	db := store.NewMemory()
	x.Store = db
	x.Agent.ExecutionConfig = nil
	x.Run.Input.Model = &sdk.RunModel{Provider: "openai", Model: firstNonEmpty(os.Getenv("NATIVE_SMOKE_MODEL"), defaultNativeOpenAIModel)}
	x.Run.Input.CredentialSource = "app"
	x.ModelCredentials = &modelauth.Manager{Store: db, Key: []byte(strings.Repeat("k", 32))}
	record, err := x.ModelCredentials.Prepare(x.Run.AppID, x.Run.ID, "openai", sdk.ModelCredential{Type: "api_key", APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateRunWithModelCredential(ctx, x.Run, nil, record); err != nil {
		t.Fatal(err)
	}
	m, err := (EinoProviderFactory{MaxTokens: 512}).ResolveNativeModel(ctx, x, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := m.Generate(ctx, NativeModelRequest{Messages: []NativeMessage{{Role: "user", Content: "Reply with exactly: run credential works"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "run credential works" {
		t.Fatal("unexpected model response")
	}
	summary, err := m.(NativeSummaryModel).Summarize(ctx, "Summarize in one short sentence: The optional run API key worked and the default key was unset.", 512)
	if err != nil || summary.Incomplete || summary.Message.Content == "" {
		t.Fatalf("summary failed: %v", err)
	}
	t.Logf("provider=%s model=%s generation_tokens=%d summary_tokens=%d", x.Run.Input.Model.Provider, x.Run.Input.Model.Model, response.Usage.OutputTokens, summary.Usage.OutputTokens)
}

func TestNativeChatGPTCredentialLiveSmoke(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_CHATGPT_LIVE_SMOKE") != "1" {
		t.Skip("opt-in live ChatGPT consent test")
	}
	var c sdk.ModelCredential
	if err := json.Unmarshal([]byte(os.Getenv("CHATGPT_SMOKE_CREDENTIAL")), &c); err != nil {
		t.Fatal("missing access credential")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	x := contextTestExec(t)
	db := store.NewMemory()
	x.Store = db
	x.Context = ctx
	x.Agent.ExecutionConfig = nil
	x.Run.Input.Model = &sdk.RunModel{Provider: "openai_chatgpt", Model: firstNonEmpty(os.Getenv("NATIVE_SMOKE_MODEL"), defaultNativeOpenAIModel)}
	x.Run.Input.CredentialSource = "app"
	// Force an expiry refresh after compilation, without expiring the real token.
	nearExpiry := time.Now().Add(25 * time.Second)
	c.ExpiresAt = &nearExpiry
	// The fixture host, not the runtime, owns and rotates the refresh token.
	x.ModelCredentials = &modelauth.Manager{Store: db, Key: []byte(strings.Repeat("k", 32)), ChatGPTEnabled: true, Callbacks: map[string]modelauth.Callback{x.Run.AppID: {URL: os.Getenv("CHATGPT_SMOKE_CALLBACK"), Token: "smoke-service"}}}
	record, err := x.ModelCredentials.Prepare(x.Run.AppID, x.Run.ID, "openai_chatgpt", c)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateRunWithModelCredential(ctx, x.Run, nil, record); err != nil {
		t.Fatal(err)
	}
	x.Agent.ApprovalMode = agentcore.ApprovalModeNever
	called := 0
	x.Tools.Register(tools.Definition{Name: "subscription_probe", Description: "Confirm the subscription test tool is callable.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		called++
		return json.RawMessage(`{"ok":true}`), nil
	})
	x.AllowedTools = map[string]bool{"subscription_probe": true}
	x.Agent.AllowedTools = []string{"subscription_probe"}
	x.Run.Input.Instructions = "Call subscription_probe exactly once, then confirm that the subscription credential works."
	result, err := executeNativeModel(ctx, x, NativeConfig{ModelFactory: EinoProviderFactory{}, MaxToolSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || result.MaxSteps {
		t.Fatal("subscription tool round trip failed")
	}
	m, err := (EinoProviderFactory{}).ResolveNativeModel(ctx, x, nil)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := m.(NativeSummaryModel).Summarize(ctx, "Summarize: The subscription fixture tool was invoked successfully.", 512)
	if err != nil || summary.Incomplete || summary.Message.Content == "" {
		t.Fatalf("subscription summary failed: %v", err)
	}
	t.Logf("subscription model=%s tool_calls=%d summary_output_tokens=%d", x.Run.Input.Model.Model, called, summary.Usage.OutputTokens)
}
