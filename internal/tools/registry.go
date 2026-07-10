package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type Definition struct {
	Name                 string      `json:"name"`
	Description          string      `json:"description"`
	Category             string      `json:"category"`
	InputSchema          interface{} `json:"input_schema"`
	Mutating             bool        `json:"mutating"`
	SupportedTargetTypes []string    `json:"supported_target_types,omitempty"`
}

type CallContext struct {
	AppID            string
	RunID            string
	Agent            *agentcore.Agent
	Run              *agentcore.AgentRun
	Target           agentcore.TargetRef
	StagedSkillRoot  string
	ArtifactWriter   ArtifactWriter
	WorkspaceManager WorkspaceManager
}

// ArtifactWriter persists run-scoped artifacts produced by tool handlers.
// It mirrors the runtime package's ArtifactWriter so callers can pass the
// same implementation through CallContext without an import cycle.
type ArtifactWriter interface {
	WriteArtifact(ctx context.Context, artifact agentcore.AgentRunArtifact) error
}

// WorkspaceManager performs runtime-owned workspace state changes requested by
// tools. Implementations must keep credentials out of model-visible outputs.
type WorkspaceManager interface {
	CheckoutRepository(ctx context.Context, req CheckoutRepositoryRequest) (*CheckoutRepositoryResult, error)
}

type CheckoutRepositoryRequest struct {
	RepositoryID string
	RepoFullName string
	BaseBranch   string
	WorkBranch   string
	Alias        string
	Primary      bool
}

type CheckoutRepositoryResult struct {
	Alias        string                    `json:"alias,omitempty"`
	Primary      bool                      `json:"primary"`
	Lease        *agentcore.WorkspaceLease `json:"lease,omitempty"`
	RepositoryID string                    `json:"repository_id,omitempty"`
	RepoFullName string                    `json:"repo_full_name,omitempty"`
	BaseBranch   string                    `json:"base_branch,omitempty"`
	WorkBranch   string                    `json:"work_branch,omitempty"`
}

type Handler func(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error)

type Registry struct {
	state *registryState
	appID string
}

type registryState struct {
	defs        map[string]Definition
	handlers    map[string]Handler
	appDefs     map[string]map[string]Definition
	appHandlers map[string]map[string]Handler
}

const (
	agentRuntimeMCPPrefix = "mcp__agent_runtime__"
	legacyMCPPrefix       = "mcp__" + "hel" + "pin" + "__"
)

func NewRegistry() *Registry {
	r := &Registry{
		state: &registryState{
			defs:        map[string]Definition{},
			handlers:    map[string]Handler{},
			appDefs:     map[string]map[string]Definition{},
			appHandlers: map[string]map[string]Handler{},
		},
	}
	r.Register(Definition{
		Name:        "get_context",
		Description: "Return the pre-resolved target context summary for the current run.",
		Category:    "Context",
		InputSchema: map[string]interface{}{
			"type":                 "object",
			"properties":           map[string]interface{}{},
			"additionalProperties": false,
		},
	}, func(_ context.Context, callCtx CallContext, _ json.RawMessage) (json.RawMessage, error) {
		if callCtx.Run == nil {
			return json.RawMessage(`{}`), nil
		}
		return json.Marshal(map[string]interface{}{
			"target":          callCtx.Run.Target,
			"context_summary": callCtx.Run.Input.ContextSummary,
		})
	})
	RegisterWorkspaceTools(r)
	RegisterRepositoryCheckoutTools(r)
	RegisterWorkspaceScanTools(r)
	RegisterArtifactPreviewTools(r)
	RegisterRepositoryProviderTools(r)
	RegisterSkillTools(r)
	RegisterWebToolsFromEnv(r)
	return r
}

// ForApp returns a registration view whose definitions and handlers are
// visible only to the specified host app. Runtime-owned tools remain global.
func (r *Registry) ForApp(appID string) *Registry {
	if r == nil {
		return nil
	}
	return &Registry{state: r.state, appID: strings.TrimSpace(appID)}
}

func (r *Registry) Register(def Definition, handler Handler) {
	if r == nil || r.state == nil {
		return
	}
	def.Name = CanonicalName(def.Name)
	if def.Name == "" {
		return
	}
	if r.appID == "" {
		r.state.defs[def.Name] = def
		if handler != nil {
			r.state.handlers[def.Name] = handler
		}
		return
	}
	if r.state.appDefs[r.appID] == nil {
		r.state.appDefs[r.appID] = map[string]Definition{}
		r.state.appHandlers[r.appID] = map[string]Handler{}
	}
	r.state.appDefs[r.appID][def.Name] = def
	if handler != nil {
		r.state.appHandlers[r.appID][def.Name] = handler
	}
}

func (r *Registry) Definitions() []Definition {
	if r == nil || r.state == nil {
		return nil
	}
	return r.DefinitionsForApp(r.appID)
}

func (r *Registry) DefinitionsForApp(appID string) []Definition {
	if r == nil || r.state == nil {
		return nil
	}
	merged := make(map[string]Definition, len(r.state.defs))
	for name, def := range r.state.defs {
		merged[name] = def
	}
	for name, def := range r.state.appDefs[strings.TrimSpace(appID)] {
		merged[name] = def
	}
	out := make([]Definition, 0, len(merged))
	for _, def := range merged {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Definition(name string) (Definition, bool) {
	return r.DefinitionForApp(r.appID, name)
}

func (r *Registry) DefinitionForApp(appID, name string) (Definition, bool) {
	if r == nil || r.state == nil {
		return Definition{}, false
	}
	name = CanonicalName(name)
	if def, ok := r.state.appDefs[strings.TrimSpace(appID)][name]; ok {
		return def, true
	}
	def, ok := r.state.defs[name]
	return def, ok
}

func (r *Registry) Execute(ctx context.Context, callCtx CallContext, name string, input json.RawMessage) (json.RawMessage, error) {
	name = CanonicalName(name)
	if r == nil || r.state == nil {
		return nil, fmt.Errorf("tool registry is not configured")
	}
	appID := strings.TrimSpace(callCtx.AppID)
	if appID == "" {
		appID = r.appID
	}
	handler := r.state.appHandlers[appID][name]
	if handler == nil {
		handler = r.state.handlers[name]
	}
	if handler == nil {
		return nil, fmt.Errorf("tool %q is not registered", name)
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return handler(ctx, callCtx, input)
}

func CanonicalName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, agentRuntimeMCPPrefix)
	name = strings.TrimPrefix(name, legacyMCPPrefix)
	switch name {
	case "request_human_input":
		return "request_user_input"
	case "request_human_approval":
		return "request_approval"
	}
	return name
}

func AllowedSet(agent *agentcore.Agent, requested []string) map[string]bool {
	set := map[string]bool{}
	if agent != nil {
		for _, tool := range agent.AllowedTools {
			tool = CanonicalName(tool)
			if tool != "" {
				set[tool] = true
			}
		}
	}
	if len(requested) == 0 {
		return set
	}
	subset := map[string]bool{}
	for _, tool := range requested {
		tool = CanonicalName(tool)
		if tool != "" && set[tool] {
			subset[tool] = true
		}
	}
	return subset
}

func ValidateAllowedSubset(agent *agentcore.Agent, requested []string) error {
	if len(requested) == 0 {
		return nil
	}
	allowed := AllowedSet(agent, nil)
	for _, tool := range requested {
		tool = CanonicalName(tool)
		if !allowed[tool] {
			return fmt.Errorf("tool %q is not allowed for agent", tool)
		}
	}
	return nil
}
