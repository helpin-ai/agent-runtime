package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/runtime"
)

type preferences struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func loadPreferences(home string) preferences {
	var p preferences
	if data, err := os.ReadFile(filepath.Join(home, "preferences.json")); err == nil {
		_ = json.Unmarshal(data, &p)
	}
	return p
}
func providerDefaults(home string) preferences {
	p := loadPreferences(home)
	if v := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_NATIVE_PROVIDER")); v != "" {
		p.Provider = v
		p.Model = ""
	}
	if v := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_NATIVE_MODEL")); v != "" {
		p.Model = v
	}
	if p.Provider == "" {
		for _, c := range runtime.NativeProviderCapabilities() {
			if c.Configured {
				p.Provider = c.Name
				break
			}
		}
	}
	if p.Model == "" {
		for _, c := range runtime.NativeProviderCapabilities() {
			if c.Name == p.Provider {
				p.Model = c.DefaultModel
			}
		}
	}
	return p
}
func savePreferences(home, provider, model string) error {
	if provider != "openai" && provider != "anthropic" && provider != "openrouter" {
		return errors.New("choose openai, anthropic, or openrouter")
	}
	return saveJSON(filepath.Join(home, "preferences.json"), preferences{provider, model})
}
