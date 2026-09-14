package appconfig

import (
	"github.com/helpin-ai/agent-runtime/internal/store"
	"strings"
	"testing"
)

func TestFreshHelpinExampleLoadsStrictPolicyAndCallback(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_APP_CONFIG", "@../../ops/examples/helpin-apps.yaml")
	t.Setenv("HELPIN_INTERNAL_API_SECRET", "test-internal-secret")
	t.Setenv("AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY", strings.Repeat("k", 32))
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Apps) != 1 || !cfg.Apps[0].RequireRunModelCredentials || cfg.Apps[0].ModelCredentialCallback == nil || cfg.Apps[0].ModelCredentialCallback.TokenEnv != "HELPIN_INTERNAL_API_SECRET" {
		t.Fatal("fresh template lost strict admission or authenticated refresh")
	}
	manager, err := ModelCredentialManager(cfg, store.NewMemory())
	if err != nil || manager == nil || manager.Callbacks["helpin"].Token != "test-internal-secret" {
		t.Fatalf("callback configuration failed: %v", err)
	}
	if cfg.Apps[0].EventProtocol != "v2" || len(cfg.Apps[0].MCPProviders) != 1 || cfg.Apps[0].WorkspaceProvider == nil {
		t.Fatal("fresh template omitted host integration")
	}
}
