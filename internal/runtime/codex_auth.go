package runtime

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const (
	codexAuthFileName          = "auth.json"
	codexAuthStateArtifactType = "codex_auth_state"
	codexAuthStateEventType    = "codex_auth.state_changed"

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
	RootDir       string
	EncryptionKey []byte
}

func NewFileCodexAuthStore(rootDir string) *FileCodexAuthStore {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		return nil
	}
	return &FileCodexAuthStore{RootDir: rootDir}
}

func NewEncryptedFileCodexAuthStore(rootDir string, encryptionKey []byte) *FileCodexAuthStore {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" || len(encryptionKey) != 32 {
		return nil
	}
	return &FileCodexAuthStore{
		RootDir:       rootDir,
		EncryptionKey: append([]byte(nil), encryptionKey...),
	}
}

func ParseCodexAuthEncryptionKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode hex codex auth encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("codex auth encryption key must be 32 bytes, got %d", len(key))
	}
	return key, nil
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
	if s.encrypted() {
		authJSON, err := decryptCodexAuthString(strings.TrimSpace(string(content)), s.EncryptionKey)
		if err != nil {
			return fmt.Errorf("decrypt codex auth: %w", err)
		}
		content = []byte(authJSON)
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
	if s.encrypted() {
		encrypted, err := encryptCodexAuthString(string(content), s.EncryptionKey)
		if err != nil {
			return fmt.Errorf("encrypt codex auth: %w", err)
		}
		content = []byte(encrypted)
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

func (s *FileCodexAuthStore) encrypted() bool {
	return s != nil && len(s.EncryptionKey) == 32
}

func encryptCodexAuthString(plaintext string, key []byte) (string, error) {
	ciphertext, err := encryptCodexAuth([]byte(plaintext), key)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptCodexAuthString(ciphertext string, key []byte) (string, error) {
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	plaintext, err := decryptCodexAuth(data, key)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func encryptCodexAuth(plaintext []byte, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return aesGCM.Seal(nonce, nonce, plaintext, nil), nil
}

func decryptCodexAuth(ciphertext []byte, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertextBytes := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertextBytes, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
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
	if execCtx.EventSink != nil && execCtx.Run != nil {
		emitCodexAuthStateEvent(ctx, execCtx.EventSink, execCtx.Run.AppID, execCtx.Run.ID, execCtx.Run.HostRunID, authState)
	}
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

func emitCodexAuthStateEvent(ctx context.Context, sink EventSink, appID, runID, hostRunID string, authState CodexAuthState) {
	if sink == nil {
		return
	}
	sink.Emit(ctx, Event{
		AppID:     strings.TrimSpace(appID),
		RunID:     strings.TrimSpace(runID),
		HostRunID: strings.TrimSpace(hostRunID),
		Type:      codexAuthStateEventType,
		Data:      codexAuthStateEventData(authState),
	})
}

func codexAuthStateEventData(authState CodexAuthState) map[string]interface{} {
	data := map[string]interface{}{
		"provider":  strings.TrimSpace(authState.Provider),
		"auth_mode": strings.TrimSpace(authState.AuthMode),
		"state":     strings.TrimSpace(authState.State),
	}
	if value := strings.TrimSpace(optionalStringValue(authState.LoginID)); value != "" {
		data["login_id"] = value
	}
	if value := strings.TrimSpace(optionalStringValue(authState.AuthURL)); value != "" {
		data["auth_url"] = value
	}
	if value := strings.TrimSpace(optionalStringValue(authState.VerificationURL)); value != "" {
		data["verification_url"] = value
	}
	if value := strings.TrimSpace(optionalStringValue(authState.UserCode)); value != "" {
		data["user_code"] = value
	}
	if value := strings.TrimSpace(optionalStringValue(authState.PlanType)); value != "" {
		data["plan_type"] = value
	}
	if value := strings.TrimSpace(optionalStringValue(authState.Error)); value != "" {
		data["error"] = value
	}
	if !authState.UpdatedAt.IsZero() {
		data["updated_at"] = authState.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return data
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
