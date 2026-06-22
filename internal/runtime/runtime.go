package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type ExecutionContext struct {
	Context           context.Context
	AppID             string
	Agent             *agentcore.Agent
	Run               *agentcore.AgentRun
	Store             agentcore.Store
	TargetContext     *host.TargetContext
	WorkspaceLease    *agentcore.WorkspaceLease
	AllowedTools      map[string]bool
	Tools             *tools.Registry
	SkillRefs         []agentcore.SkillRef
	SkillDefinitions  []skills.Definition
	SkillInstructions string
	SkillPolicy       skills.Policy
	StagedSkillRoot   string
	ArtifactWriter    ArtifactWriter
	InteractionBroker InteractionBroker
	EventSink         EventSink
}

type ArtifactWriter interface {
	WriteArtifact(ctx context.Context, artifact agentcore.AgentRunArtifact) error
}

type InteractionBroker interface {
	RequestInteraction(ctx context.Context, interaction agentcore.AgentRunInteraction) error
}

type EventSink interface {
	Emit(ctx context.Context, event Event)
}

type Event struct {
	AppID string                 `json:"app_id"`
	RunID string                 `json:"run_id"`
	Type  string                 `json:"type"`
	Data  map[string]interface{} `json:"data,omitempty"`
}

type Result struct {
	AssistantMessage  string
	OutputSummary     json.RawMessage
	WaitForApproval   bool
	AwaitingInput     bool
	AwaitingAuth      bool
	MessagesPersisted bool
}

type Adapter interface {
	Kind() string
	Execute(ctx *ExecutionContext) (*Result, error)
}

type Registry struct {
	adapters map[string]Adapter
}

func NewRegistry(adapters ...Adapter) *Registry {
	registry := &Registry{adapters: map[string]Adapter{}}
	for _, adapter := range adapters {
		if adapter != nil {
			registry.adapters[adapter.Kind()] = adapter
		}
	}
	return registry
}

func (r *Registry) Get(kind string) (Adapter, error) {
	if r == nil {
		return nil, fmt.Errorf("runtime registry is not configured")
	}
	adapter := r.adapters[kind]
	if adapter == nil {
		return nil, fmt.Errorf("runtime adapter %q is not configured", kind)
	}
	return adapter, nil
}
