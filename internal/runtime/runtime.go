package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type ExecutionContext struct {
	ModelCredentials          *modelauth.Manager
	Context                   context.Context
	AppID                     string
	Agent                     *agentcore.Agent
	Run                       *agentcore.AgentRun
	Store                     agentcore.Store
	TargetContext             *host.TargetContext
	WorkspaceLease            *agentcore.WorkspaceLease
	AllowedTools              map[string]bool
	Tools                     *tools.Registry
	WorkspaceManager          tools.WorkspaceManager
	SkillRefs                 []agentcore.SkillRef
	SkillDefinitions          []skills.Definition
	AvailableSkillRefs        []agentcore.SkillRef
	AvailableSkillDefinitions []skills.Definition
	SkillInstructions         string
	SkillPolicy               skills.Policy
	UsesSplitSkills           bool
	StagedSkillRoot           string
	ArtifactWriter            ArtifactWriter
	InteractionBroker         InteractionBroker
	EventSink                 EventSink
	MCPBrokerURL              string
	MCPBrokerToken            string
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

type Event = sdk.Event

type Result struct {
	AssistantMessage      string
	AssistantMessageID    string
	OutputSummary         json.RawMessage
	WaitForApproval       bool
	AwaitingInput         bool
	AwaitingAuth          bool
	MessagesPersisted     bool
	TurnFinished          bool
	TurnOutcome           string
	CompletionCorrections int
}

type Adapter interface {
	Kind() string
	Execute(ctx *ExecutionContext) (*Result, error)
}

// RemoteCanceler is implemented by adapters whose work continues on another
// service. Engine.CancelRun asks them to stop that work; worker shutdown does
// not, because a durable retry resumes the same remote work.
type RemoteCanceler interface {
	NeedsRemoteCancel(run *agentcore.AgentRun) bool
	CancelRemote(ctx context.Context, run *agentcore.AgentRun, targetContext *host.TargetContext) error
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
