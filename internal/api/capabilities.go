package api

import (
	"net/http"

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

func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	var defs []tools.Definition
	if s.cfg.Tools != nil {
		defs = s.cfg.Tools.Definitions()
	}
	writeJSON(w, http.StatusOK, capabilitiesResponse{
		Capabilities:       s.cfg.Capabilities,
		ServiceAuthEnabled: s.cfg.ServiceToken != "",
		Tools:              defs,
	})
}
