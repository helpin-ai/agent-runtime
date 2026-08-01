package api

import (
	"net/http"

	"github.com/helpin-ai/agent-runtime/internal/appconfig"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Capabilities is a read-only, non-secret snapshot of how the runtime is
// configured. It is assembled at startup (where env/app-config is known) and
// injected into the server; the tools list is served live from the registry.
type Capabilities struct {
	RuntimeKinds []string                     `json:"runtime_kinds"`
	Providers    []runtime.ProviderCapability `json:"providers"`
	Store        StoreInfo                    `json:"store"`
	Durable      DurableInfo                  `json:"durable"`
	Skills       []SkillInfo                  `json:"skills,omitempty"`
	Apps         []appconfig.AppSummary       `json:"apps,omitempty"`
	RunMCP       RunMCPCapability             `json:"run_mcp"`
}

type RunMCPCapability struct {
	Supported                      bool     `json:"supported"`
	Transports                     []string `json:"transports"`
	CredentialEncryptionConfigured bool     `json:"credential_encryption_configured"`
}

func (s *Server) appHealth(w http.ResponseWriter, r *http.Request) {
	appID := r.URL.Query().Get("app_id")
	health, err := appconfig.CheckApp(r.Context(), s.cfg.AppConfig, appID, nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, health)
}

type StoreInfo struct {
	Driver   string `json:"driver"`
	InMemory bool   `json:"in_memory"`
}

type DurableInfo struct {
	Enabled         bool   `json:"enabled"`
	TemporalAddress string `json:"temporal_address,omitempty"`
	Namespace       string `json:"namespace,omitempty"`
}

type SkillInfo struct {
	Key         string `json:"key"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// capabilitiesResponse is the wire shape: the injected snapshot plus values
// known only to the server (live tools, whether service auth is enforced).
type capabilitiesResponse struct {
	Capabilities
	ServiceAuthEnabled bool               `json:"service_auth_enabled"`
	Tools              []tools.Definition `json:"tools"`
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	capabilities := s.cfg.Capabilities
	if appID := r.URL.Query().Get("app_id"); appID != "" {
		capabilities.Apps = nil
		for _, app := range s.cfg.Capabilities.Apps {
			if app.AppID == appID {
				capabilities.Apps = []appconfig.AppSummary{app}
				break
			}
		}
	}
	var defs []tools.Definition
	if s.cfg.Tools != nil {
		defs = s.cfg.Tools.DefinitionsForApp(r.URL.Query().Get("app_id"))
	}
	writeJSON(w, http.StatusOK, capabilitiesResponse{
		Capabilities:       capabilities,
		ServiceAuthEnabled: s.cfg.ServiceToken != "",
		Tools:              defs,
	})
}
