package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestFileCodexAuthStorePromoteRestoreClear(t *testing.T) {
	tmp := t.TempDir()
	authStore := NewFileCodexAuthStore(filepath.Join(tmp, "auth-store"))
	scope := CodexAuthScope{
		AppID:    "app-a",
		TenantID: "tenant-a",
		Provider: "openai",
		AuthMode: codexOpenAIAuthModeDevice,
	}
	sessionHome := filepath.Join(tmp, "session", ".codex")
	if err := writeCodexAuthFile(filepath.Join(sessionHome, codexAuthFileName), `{"refresh_token":"secret"}`); err != nil {
		t.Fatalf("write session auth: %v", err)
	}
	if err := authStore.Promote(context.Background(), scope, sessionHome); err != nil {
		t.Fatalf("promote: %v", err)
	}
	restoreHome := filepath.Join(tmp, "restore", ".codex")
	if err := authStore.Restore(context.Background(), scope, restoreHome); err != nil {
		t.Fatalf("restore: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(restoreHome, codexAuthFileName))
	if err != nil {
		t.Fatalf("read restored auth: %v", err)
	}
	if string(content) != `{"refresh_token":"secret"}` {
		t.Fatalf("unexpected restored auth: %s", string(content))
	}
	if err := authStore.Clear(context.Background(), scope); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := authStore.Restore(context.Background(), scope, filepath.Join(tmp, "empty", ".codex")); err != nil {
		t.Fatalf("restore after clear: %v", err)
	}
}

func TestCodexAdapterAppServerRequiresDeviceAuth(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"requiresOpenaiAuth":true}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-auth",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Instructions: "change code", Metadata: map[string]interface{}{"tenant_id": "tenant-a"}},
		RuntimeKind: agentcore.RuntimeCodex,
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath:    command,
		WorkDir:        tmp,
		Timeout:        time.Second,
		AppServer:      true,
		OpenAIAuthMode: codexOpenAIAuthModeDevice,
		RuntimeRoot:    filepath.Join(tmp, "runtime"),
	})
	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run:     run,
		ArtifactWriter: testArtifactWriter{
			store: mem,
			run:   run,
		},
		InteractionBroker: testInteractionBroker{
			store: mem,
			run:   run,
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result == nil || !result.AwaitingAuth {
		t.Fatalf("expected auth wait, got %#v", result)
	}
	artifacts, err := mem.ListArtifacts(context.Background(), "app-a", "run-auth")
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	foundAuth := false
	foundSession := false
	for _, artifact := range artifacts {
		if artifact.ArtifactType == codexAuthStateArtifactType && strings.Contains(artifact.InlineContent, `"state":"required"`) {
			foundAuth = true
		}
		if artifact.ArtifactType == codexSessionStateArtifactType && strings.Contains(artifact.InlineContent, `"codex_home"`) {
			foundSession = true
		}
	}
	if !foundAuth || !foundSession {
		t.Fatalf("expected auth and session artifacts, got %#v", artifacts)
	}
	interactions, err := mem.ListInteractions(context.Background(), "app-a", "run-auth")
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != "authentication" {
		t.Fatalf("expected authentication interaction, got %#v", interactions)
	}
}

type testArtifactWriter struct {
	store agentcore.Store
	run   *agentcore.AgentRun
}

func (w testArtifactWriter) WriteArtifact(ctx context.Context, artifact agentcore.AgentRunArtifact) error {
	artifact.AppID = w.run.AppID
	artifact.RunID = w.run.ID
	return w.store.AppendArtifact(ctx, &artifact)
}
