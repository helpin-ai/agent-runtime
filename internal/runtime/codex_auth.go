package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const (
	codexAuthFileName          = "auth.json"
	codexAuthStateArtifactType = "codex_auth_state"

	codexOpenAIAuthModeAPIKey = "api_key"
	codexOpenAIAuthModeOAuth  = "chatgpt_oauth"
	codexOpenAIAuthModeDevice = "chatgpt_device_code"

	codexAuthStateRequired  = "required"
	codexAuthStatePending   = "pending"
	codexAuthStateConnected = "connected"
	codexAuthStateFailed    = "failed"
	codexAuthStateCancelled = "cancelled"
)

type CodexAuthStore interface {
	Restore(ctx context.Context, scope CodexAuthScope, codexHome string) error
	Promote(ctx context.Context, scope CodexAuthScope, codexHome string) error
	Clear(ctx context.Context, scope CodexAuthScope) error
}

type CodexAuthScope struct {
	AppID    string `json:"app_id"`
	TenantID string `json:"tenant_id,omitempty"`
	Provider string `json:"provider"`
	AuthMode string `json:"auth_mode"`
}

type CodexAuthState struct {
	Provider        string    `json:"provider,omitempty"`
	AuthMode        string    `json:"auth_mode,omitempty"`
	State           string    `json:"state"`
	LoginID         *string   `json:"login_id,omitempty"`
	AuthURL         *string   `json:"auth_url,omitempty"`
	VerificationURL *string   `json:"verification_url,omitempty"`
	UserCode        *string   `json:"user_code,omitempty"`
	PlanType        *string   `json:"plan_type,omitempty"`
	Error           *string   `json:"error,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type FileCodexAuthStore struct {
	RootDir string
}

func NewFileCodexAuthStore(rootDir string) *FileCodexAuthStore {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		return nil
	}
	return &FileCodexAuthStore{RootDir: rootDir}
}

func (s *FileCodexAuthStore) Restore(_ context.Context, scope CodexAuthScope, codexHome string) error {
	if s == nil || !codexShouldPersistAuth(scope.Provider, scope.AuthMode) || strings.TrimSpace(codexHome) == "" {
		return nil
	}
	content, err := os.ReadFile(s.scopePath(scope))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return writeCodexAuthFile(filepath.Join(strings.TrimSpace(codexHome), codexAuthFileName), string(content))
}

func (s *FileCodexAuthStore) Promote(_ context.Context, scope CodexAuthScope, codexHome string) error {
	if s == nil || !codexShouldPersistAuth(scope.Provider, scope.AuthMode) || strings.TrimSpace(codexHome) == "" {
		return nil
	}
	content, err := os.ReadFile(filepath.Join(strings.TrimSpace(codexHome), codexAuthFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read session codex auth: %w", err)
	}
	path := s.scopePath(scope)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o600)
}

func (s *FileCodexAuthStore) Clear(_ context.Context, scope CodexAuthScope) error {
	if s == nil {
		return nil
	}
	if err := os.Remove(s.scopePath(scope)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *FileCodexAuthStore) scopePath(scope CodexAuthScope) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(scope.AppID),
		strings.TrimSpace(scope.TenantID),
		strings.TrimSpace(scope.Provider),
		strings.TrimSpace(scope.AuthMode),
	}, "\x00")))
	return filepath.Join(strings.TrimSpace(s.RootDir), hex.EncodeToString(sum[:])+".json")
}

func codexShouldPersistAuth(provider, authMode string) bool {
	return strings.TrimSpace(provider) == "openai" && strings.TrimSpace(authMode) == codexOpenAIAuthModeDevice
}

func codexAuthScope(execCtx *ExecutionContext, provider, authMode string) CodexAuthScope {
	scope := CodexAuthScope{
		Provider: strings.TrimSpace(provider),
		AuthMode: strings.TrimSpace(authMode),
	}
	if execCtx == nil || execCtx.Run == nil {
		return scope
	}
	scope.AppID = strings.TrimSpace(execCtx.Run.AppID)
	scope.TenantID = strings.TrimSpace(stringMetadata(execCtx.Run.Input.Metadata, "tenant_id"))
	if scope.TenantID == "" {
		scope.TenantID = strings.TrimSpace(stringMetadata(execCtx.Run.Input.Metadata, "workspace_id"))
	}
	if scope.TenantID == "" {
		scope.TenantID = scope.AppID
	}
	return scope
}

func codexAuthState(provider, authMode, state string) CodexAuthState {
	return CodexAuthState{
		Provider:  strings.TrimSpace(provider),
		AuthMode:  strings.TrimSpace(authMode),
		State:     strings.TrimSpace(state),
		UpdatedAt: time.Now().UTC(),
	}
}

func writeCodexAuthFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func persistCodexAuthState(ctx context.Context, execCtx *ExecutionContext, authState CodexAuthState) {
	if execCtx == nil || execCtx.ArtifactWriter == nil {
		return
	}
	_ = execCtx.ArtifactWriter.WriteArtifact(ctx, codexAuthStateArtifact(authState))
}

func appendCodexAuthState(ctx context.Context, store agentcore.Store, appID, runID string, authState CodexAuthState) error {
	if store == nil {
		return nil
	}
	artifact := codexAuthStateArtifact(authState)
	artifact.AppID = appID
	artifact.RunID = runID
	return store.AppendArtifact(ctx, &artifact)
}

func codexAuthStateArtifact(authState CodexAuthState) agentcore.AgentRunArtifact {
	payload, _ := json.Marshal(authState)
	return agentcore.AgentRunArtifact{
		ArtifactType:  codexAuthStateArtifactType,
		Format:        "json",
		StorageMode:   "inline",
		InlineContent: string(payload),
		Metadata:      json.RawMessage(`{"internal":false}`),
	}
}

func requestCodexAuthInteraction(ctx context.Context, execCtx *ExecutionContext, authState CodexAuthState) {
	if execCtx == nil || execCtx.InteractionBroker == nil {
		return
	}
	payload, _ := json.Marshal(authState)
	_ = execCtx.InteractionBroker.RequestInteraction(ctx, agentcore.AgentRunInteraction{
		InteractionKind: "authentication",
		Status:          "pending",
		Title:           "Codex authentication required",
		Summary:         "Sign in with ChatGPT to continue this Codex run.",
		RequestPayload:  payload,
	})
}

func stringMetadata(metadata map[string]interface{}, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return value
}
