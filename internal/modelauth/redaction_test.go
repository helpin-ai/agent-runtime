package modelauth

import (
	"context"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"strings"
	"testing"
)

func TestReviewContextRedactsRunAndCallbackCredentials(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	m := &Manager{Store: db, Key: []byte(strings.Repeat("k", 32)), Callbacks: map[string]Callback{"app": {Token: "callback-secret"}}}
	record, err := m.Prepare("app", "run", "openai", sdk.ModelCredential{Type: "api_key", APIKey: "model-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRunWithModelCredential(ctx, &agentcore.AgentRun{AppID: "app", ID: "run", Status: agentcore.RunStatusRunning}, nil, record); err != nil {
		t.Fatal(err)
	}
	got, err := m.RedactReviewContext(ctx, "app", "run", "model-secret callback-secret ordinary context")
	if err != nil {
		t.Fatal(err)
	}
	if got != "[REDACTED] [REDACTED] ordinary context" {
		t.Fatalf("unsafe context: %s", got)
	}
	m.Key = []byte(strings.Repeat("x", 32))
	if _, err := m.RedactReviewContext(ctx, "app", "run", "context"); err == nil {
		t.Fatal("decrypt failure permitted external review")
	}
}

func TestRedactKnownSecretMasksLiteralAndJSONEscapedForms(t *testing.T) {
	secret := "line\nsecret"
	got := RedactKnownSecret("literal="+secret+` escaped=line\nsecret`, secret)
	if strings.Contains(got, "line") || strings.Contains(got, "secret") {
		t.Fatalf("secret remained in redacted value: %q", got)
	}
}
