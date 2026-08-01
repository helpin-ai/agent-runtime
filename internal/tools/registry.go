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
	AppID           string
	RunID           string
	Agent           *agentcore.Agent
	Run             *agentcore.AgentRun
	Target          agentcore.TargetRef
	StagedSkillRoot string
}

type Handler func(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error)

type Registry struct {
	defs     map[string]Definition
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	r := &Registry{
		defs:     map[string]Definition{},
		handlers: map[string]Handler{},
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
	RegisterSkillTools(r)
	RegisterWebToolsFromEnv(r)
	return r
}

func (r *Registry) Register(def Definition, handler Handler) {
	if r == nil {
		return
	}
	def.Name = CanonicalName(def.Name)
	if def.Name == "" {
		return
	}
	r.defs[def.Name] = def
	if handler != nil {
		r.handlers[def.Name] = handler
	}
}

func (r *Registry) Definitions() []Definition {
	if r == nil {
		return nil
	}
	out := make([]Definition, 0, len(r.defs))
	for _, def := range r.defs {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Definition(name string) (Definition, bool) {
	if r == nil {
		return Definition{}, false
	}
	def, ok := r.defs[CanonicalName(name)]
	return def, ok
}

func (r *Registry) Execute(ctx context.Context, callCtx CallContext, name string, input json.RawMessage) (json.RawMessage, error) {
	name = CanonicalName(name)
	if r == nil {
		return nil, fmt.Errorf("tool registry is not configured")
	}
	handler := r.handlers[name]
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
	// Codex reports MCP calls either as mcp__<server>__<tool> names or as
	// <server>/<tool> app-server items. Skill and allow-list contracts use the
	// logical tool name, so discard only the transport namespace while keeping
	// the complete tool portion (including any double underscores it contains).
	if strings.HasPrefix(name, "mcp__") {
		qualified := strings.TrimPrefix(name, "mcp__")
		if separator := strings.Index(qualified, "__"); separator >= 0 && separator+2 < len(qualified) {
			name = qualified[separator+2:]
		}
	}
	if separator := strings.Index(name, "/"); separator >= 0 && separator+1 < len(name) {
		name = name[separator+1:]
	}
	name = strings.TrimSpace(name)
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
