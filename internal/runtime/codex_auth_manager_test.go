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

func TestCodexAuthManagerStartDeviceCodePromotesAuthOnConnected(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"requiresOpenaiAuth":true}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"type":"chatgptDeviceCode","loginId":"login-1","verificationUrl":"https://example.test/device","userCode":"ABCD"}}'
mkdir -p "$CODEX_HOME"
printf '%s' '{"refresh_token":"device-secret"}' > "$CODEX_HOME/auth.json"
printf '%s\n' '{"method":"account/login/completed","params":{"loginId":"login-1","success":true}}'
printf '%s\n' '{"method":"account/updated","params":{"authMode":"chatgpt","planType":"plus"}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	agent := &agentcore.Agent{
		ID:          "agent-1",
		AppID:       "app-a",
		Name:        "Codex",
		RuntimeKind: agentcore.RuntimeCodex,
		Provider:    "openai",
		Model:       "gpt",
	}
	if err := mem.CreateAgent(context.Background(), agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		ID:          "run-1",
		AppID:       "app-a",
		AgentID:     agent.ID,
		RuntimeKind: agentcore.RuntimeCodex,
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Metadata: map[string]interface{}{"tenant_id": "tenant-a"}},
	}
	if err := mem.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	authStore := NewEncryptedFileCodexAuthStore(filepath.Join(tmp, "auth-store"), []byte("12345678901234567890123456789012"))
	if authStore == nil {
		t.Fatal("expected encrypted auth store")
	}
	manager := NewCodexAuthManager(mem, CodexConfig{
		CommandPath:    command,
		Timeout:        time.Second,
		OpenAIAuthMode: codexOpenAIAuthModeDevice,
		RuntimeRoot:    filepath.Join(tmp, "runtime"),
		AuthStore:      authStore,
	})
	state, err := manager.StartDeviceCode(context.Background(), "app-a", "run-1")
	if err != nil {
		t.Fatalf("start device code: %v", err)
	}
	if state.State != codexAuthStatePending || state.UserCode == nil || *state.UserCode != "ABCD" {
		t.Fatalf("unexpected pending state: %#v", state)
	}
	waitForAuthArtifact(t, mem, "app-a", "run-1", codexAuthStateConnected)
	scope := CodexAuthScope{
		AppID:    "app-a",
		TenantID: "tenant-a",
		Provider: "openai",
		AuthMode: codexOpenAIAuthModeDevice,
	}
	stored, err := os.ReadFile(authStore.scopePath(scope))
	if err != nil {
		t.Fatalf("read stored auth: %v", err)
	}
	if strings.Contains(string(stored), "device-secret") || strings.Contains(string(stored), "refresh_token") {
		t.Fatalf("stored auth was not encrypted: %s", string(stored))
	}
	restoreHome := filepath.Join(tmp, "restore", ".codex")
	if err := authStore.Restore(context.Background(), scope, restoreHome); err != nil {
		t.Fatalf("restore promoted auth: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(restoreHome, codexAuthFileName))
	if err != nil {
		t.Fatalf("read restored auth: %v", err)
	}
	if string(content) != `{"refresh_token":"device-secret"}` {
		t.Fatalf("unexpected restored auth: %s", string(content))
	}
}

func waitForAuthArtifact(t *testing.T, mem *store.Memory, appID, runID, state string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		artifacts, err := mem.ListArtifacts(context.Background(), appID, runID)
		if err != nil {
			t.Fatalf("list artifacts: %v", err)
		}
		for _, artifact := range artifacts {
			if artifact.ArtifactType == codexAuthStateArtifactType && strings.Contains(artifact.InlineContent, `"state":"`+state+`"`) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("did not observe auth state %q", state)
}
