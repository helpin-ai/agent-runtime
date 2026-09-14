package appconfig

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
)

type ModelCredentialCallback struct {
	URL      string `json:"url" yaml:"url"`
	TokenEnv string `json:"token_env" yaml:"token_env"`
}

// RequiresRunModelCredentials reads trusted per-app policy. Omitted policy
// preserves standalone and existing host integrations' default-key behavior.
func RequiresRunModelCredentials(cfg *Config, appID string) bool {
	if cfg == nil {
		return false
	}
	for _, app := range cfg.Apps {
		if app.AppID == strings.TrimSpace(appID) {
			return app.RequireRunModelCredentials
		}
	}
	return false
}

func ModelCredentialManager(cfg *Config, store agentcore.Store) (*modelauth.Manager, error) {
	raw := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_MODEL_CREDENTIAL_ENCRYPTION_KEY"))
	if raw == "" {
		return nil, nil
	}
	key, err := mcp.ParseCredentialEncryptionKey(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid model credential encryption key")
	}
	credentialStore, ok := store.(agentcore.ModelCredentialStore)
	if !ok {
		return nil, fmt.Errorf("store does not support model credentials")
	}
	manager := &modelauth.Manager{Store: credentialStore, Key: key, ChatGPTEnabled: strings.EqualFold(os.Getenv("AGENT_RUNTIME_CHATGPT_ENABLED"), "true"), Callbacks: map[string]modelauth.Callback{}}
	if cfg != nil {
		for _, app := range cfg.Apps {
			if callback := app.ModelCredentialCallback; callback != nil {
				parsed, err := url.Parse(callback.URL)
				if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1"))) {
					return nil, fmt.Errorf("invalid model credential callback URL for app %s", app.AppID)
				}
				token := os.Getenv(callback.TokenEnv)
				if token == "" {
					return nil, fmt.Errorf("model credential callback token is missing for app %s", app.AppID)
				}
				manager.Callbacks[app.AppID] = modelauth.Callback{URL: callback.URL, Token: token}
			}
		}
	}
	return manager, nil
}
