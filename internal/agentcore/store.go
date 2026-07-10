package agentcore

import "context"

type Store interface {
	CreateAgent(ctx context.Context, agent *Agent) error
	GetAgent(ctx context.Context, appID, agentID string) (*Agent, error)
	ListAgents(ctx context.Context, appID string) ([]Agent, error)
	UpdateAgent(ctx context.Context, agent *Agent) error

	CreateRun(ctx context.Context, run *AgentRun) error
	GetRun(ctx context.Context, appID, runID string) (*AgentRun, error)
	GetRunByHostRunID(ctx context.Context, appID, hostRunID string) (*AgentRun, error)
	ListRuns(ctx context.Context, appID string) ([]AgentRun, error)
	ListRunsByStatus(ctx context.Context, statuses ...string) ([]AgentRun, error)
	SearchRuns(ctx context.Context, search RunSearch) (*RunPage, error)
	UpdateRun(ctx context.Context, run *AgentRun) error

	AppendMessage(ctx context.Context, message *AgentRunMessage) error
	ListMessages(ctx context.Context, appID, runID string) ([]AgentRunMessage, error)
	AppendArtifact(ctx context.Context, artifact *AgentRunArtifact) error
	ListArtifacts(ctx context.Context, appID, runID string) ([]AgentRunArtifact, error)
	AppendInteraction(ctx context.Context, interaction *AgentRunInteraction) error
	ListInteractions(ctx context.Context, appID, runID string) ([]AgentRunInteraction, error)
	UpdateInteraction(ctx context.Context, interaction *AgentRunInteraction) error
	AppendToolCall(ctx context.Context, call *ToolCall) error
	ListToolCalls(ctx context.Context, appID, runID string) ([]ToolCall, error)
	AppendEvent(ctx context.Context, event *AgentRunEvent) error
	ListEvents(ctx context.Context, appID, runID string) ([]AgentRunEvent, error)
}
