package appconfig

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
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
				if !validModelCallbackURL(callback.URL, strings.EqualFold(os.Getenv("AGENT_RUNTIME_MODEL_CALLBACK_ALLOW_HTTP"), "true"), os.Getenv("AGENT_RUNTIME_MODEL_CALLBACK_HTTP_HOSTS")) {
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

// validModelCallbackURL permits a private Compose callback only when both the
// operator opt-in and exact host:port entry match. It is unrelated to run-supplied
// MCP network policy, and never accepts wildcard/domain-suffix allowances.
func validModelCallbackURL(raw string, allowHTTP bool, hosts string) bool {
	u, err := url.Parse(raw)
	if err != nil || raw != strings.TrimSpace(raw) || u == nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(u.Host, "* ,\\") || strings.Contains(raw, "#") {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	if u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" {
		return true
	}
	if !allowHTTP {
		return false
	}
	for _, entry := range strings.Split(hosts, ",") {
		entry = strings.TrimSpace(entry)
		host, port, err := net.SplitHostPort(entry)
		if err != nil || host == "" || strings.ContainsAny(host, "*/ \\@") {
			continue
		}
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			continue
		}
		if strings.EqualFold(u.Host, entry) {
			return true
		}
	}
	return false
}
