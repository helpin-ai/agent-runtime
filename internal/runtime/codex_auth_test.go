package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
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

func TestEncryptedFileCodexAuthStorePromoteRestoreClear(t *testing.T) {
	tmp := t.TempDir()
	key := []byte("12345678901234567890123456789012")
	authStore := NewEncryptedFileCodexAuthStore(filepath.Join(tmp, "auth-store"), key)
	if authStore == nil {
		t.Fatal("expected encrypted auth store")
	}
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
	stored, err := os.ReadFile(authStore.scopePath(scope))
	if err != nil {
		t.Fatalf("read stored auth: %v", err)
	}
	if strings.Contains(string(stored), "refresh_token") || strings.Contains(string(stored), "secret") {
		t.Fatalf("stored auth was not encrypted: %s", string(stored))
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

func TestEncryptedFileCodexAuthStoreRejectsInvalidKey(t *testing.T) {
	if store := NewEncryptedFileCodexAuthStore(t.TempDir(), []byte("too-short")); store != nil {
		t.Fatalf("expected invalid encrypted auth store to be nil")
	}
	if _, err := ParseCodexAuthEncryptionKey("not-hex"); err == nil {
		t.Fatalf("expected invalid hex key error")
	}
	if _, err := ParseCodexAuthEncryptionKey(hex.EncodeToString([]byte("too-short"))); err == nil {
		t.Fatalf("expected short key error")
	}
}

func TestStoreBackedCodexAuthStorePromoteRestoreClearAcrossStores(t *testing.T) {
	key := []byte("12345678901234567890123456789012")
	dsn := fmt.Sprintf("file:codex_auth_store_%d?mode=memory&cache=shared", time.Now().UnixNano())
	sqlStoreA := openTestSQLStore(t, dsn)
	sqlStoreB := openTestSQLStore(t, dsn)
	authStoreA := NewStoreBackedCodexAuthStore(sqlStoreA.DB(), key)
	authStoreB := NewStoreBackedCodexAuthStore(sqlStoreB.DB(), key)
	if authStoreA == nil || authStoreB == nil {
		t.Fatal("expected store-backed auth stores")
	}
	scope := CodexAuthScope{
		AppID:    "app-a",
		TenantID: "tenant-a",
		Provider: "openai",
		AuthMode: codexOpenAIAuthModeDevice,
	}
	tmp := t.TempDir()
	sessionHome := filepath.Join(tmp, "api-pod", ".codex")
	if err := writeCodexAuthFile(filepath.Join(sessionHome, codexAuthFileName), `{"refresh_token":"secret"}`); err != nil {
		t.Fatalf("write session auth: %v", err)
	}
	if err := authStoreA.Promote(context.Background(), scope, sessionHome); err != nil {
		t.Fatalf("promote: %v", err)
	}
	var record struct {
		Payload []byte
	}
	if err := sqlStoreA.DB().Table("codex_auth_tokens").Where("app_id = ? AND tenant_id = ? AND provider = ? AND auth_mode = ?", scope.AppID, scope.TenantID, scope.Provider, scope.AuthMode).First(&record).Error; err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if strings.Contains(string(record.Payload), "refresh_token") || strings.Contains(string(record.Payload), "secret") {
		t.Fatalf("stored auth was not encrypted: %q", string(record.Payload))
	}
	restoreHome := filepath.Join(tmp, "worker-pod", ".codex")
	if err := authStoreB.Restore(context.Background(), scope, restoreHome); err != nil {
		t.Fatalf("restore: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(restoreHome, codexAuthFileName))
	if err != nil {
		t.Fatalf("read restored auth: %v", err)
	}
	if string(content) != `{"refresh_token":"secret"}` {
		t.Fatalf("unexpected restored auth: %s", string(content))
	}
	if err := authStoreB.Clear(context.Background(), scope); err != nil {
		t.Fatalf("clear: %v", err)
	}
	var count int64
	if err := sqlStoreA.DB().Table("codex_auth_tokens").Count(&count).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected clear to delete row, count=%d", count)
	}
}

func TestStoreBackedCodexAuthStoreWrongKeyRestoresAsNoToken(t *testing.T) {
	key := []byte("12345678901234567890123456789012")
	wrongKey := []byte("abcdefghijklmnopqrstuvwxy1234567")
	sqlStore := openTestSQLStore(t, fmt.Sprintf("file:codex_auth_wrong_key_%d?mode=memory&cache=shared", time.Now().UnixNano()))
	authStore := NewStoreBackedCodexAuthStore(sqlStore.DB(), key)
	wrongKeyStore := NewStoreBackedCodexAuthStore(sqlStore.DB(), wrongKey)
	scope := CodexAuthScope{
		AppID:    "app-a",
		TenantID: "tenant-a",
		Provider: "openai",
		AuthMode: codexOpenAIAuthModeDevice,
	}
	tmp := t.TempDir()
	sessionHome := filepath.Join(tmp, "session", ".codex")
	if err := writeCodexAuthFile(filepath.Join(sessionHome, codexAuthFileName), `{"refresh_token":"secret"}`); err != nil {
		t.Fatalf("write session auth: %v", err)
	}
	if err := authStore.Promote(context.Background(), scope, sessionHome); err != nil {
		t.Fatalf("promote: %v", err)
	}
	restoreHome := filepath.Join(tmp, "restore", ".codex")
	if err := writeCodexAuthFile(filepath.Join(restoreHome, codexAuthFileName), `{"refresh_token":"stale"}`); err != nil {
		t.Fatalf("write stale auth: %v", err)
	}
	if err := wrongKeyStore.Restore(context.Background(), scope, restoreHome); err != nil {
		t.Fatalf("restore with wrong key should not error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restoreHome, codexAuthFileName)); !os.IsNotExist(err) {
		t.Fatalf("expected wrong-key restore to remove stale auth, err=%v", err)
	}
}

func TestStoreBackedCodexAuthStoreRejectsInvalidKey(t *testing.T) {
	sqlStore := openTestSQLStore(t, fmt.Sprintf("file:codex_auth_invalid_key_%d?mode=memory&cache=shared", time.Now().UnixNano()))
	if authStore := NewStoreBackedCodexAuthStore(sqlStore.DB(), []byte("too-short")); authStore != nil {
		t.Fatalf("expected invalid store-backed auth store to be nil")
	}
}

func TestDefaultCodexConfigFromEnvRequiresEncryptedAuthStore(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_CODEX_AUTH_DIR", filepath.Join(t.TempDir(), "auth-store"))
	t.Setenv("AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY", "")
	t.Setenv("CODEX_AUTH_ENCRYPTION_KEY", "")
	if cfg := DefaultCodexConfigFromEnv(); cfg.AuthStore != nil {
		t.Fatalf("expected auth store to be disabled without encryption key")
	}

	key := []byte("12345678901234567890123456789012")
	t.Setenv("AGENT_RUNTIME_CODEX_AUTH_ENCRYPTION_KEY", hex.EncodeToString(key))
	cfg := DefaultCodexConfigFromEnv()
	store, ok := cfg.AuthStore.(*FileCodexAuthStore)
	if !ok || store == nil || !store.encrypted() {
		t.Fatalf("expected encrypted file auth store, got %#v", cfg.AuthStore)
	}
}

func TestDefaultCodexConfigFromEnvReadsSandboxAndApprovalSettings(t *testing.T) {
	t.Setenv("CODEX_SANDBOX_MODE", "danger-full-access")
	t.Setenv("CODEX_APPROVAL_POLICY", "never")
	t.Setenv("CODEX_APPROVALS_REVIEWER", "auto_review")

	cfg := DefaultCodexConfigFromEnv()
	if cfg.Sandbox != "danger-full-access" {
		t.Fatalf("Sandbox = %q", cfg.Sandbox)
	}
	if cfg.ApprovalPolicy != "never" {
		t.Fatalf("ApprovalPolicy = %q", cfg.ApprovalPolicy)
	}
	if cfg.ApprovalsReviewer != "auto_review" {
		t.Fatalf("ApprovalsReviewer = %q", cfg.ApprovalsReviewer)
	}
}

func openTestSQLStore(t *testing.T, dsn string) *store.SQL {
	t.Helper()
	sqlStore, err := store.OpenSQL(store.SQLConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open sql store: %v", err)
	}
	if err := sqlStore.AutoMigrate(); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return sqlStore
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
