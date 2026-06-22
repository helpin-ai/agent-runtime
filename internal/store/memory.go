package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/id"
)

type Memory struct {
	mu           sync.RWMutex
	agents       map[string]*agentcore.Agent
	runs         map[string]*agentcore.AgentRun
	messages     map[string][]agentcore.AgentRunMessage
	artifacts    map[string][]agentcore.AgentRunArtifact
	interactions map[string][]agentcore.AgentRunInteraction
	toolCalls    map[string][]agentcore.ToolCall
}

func NewMemory() *Memory {
	return &Memory{
		agents:       map[string]*agentcore.Agent{},
		runs:         map[string]*agentcore.AgentRun{},
		messages:     map[string][]agentcore.AgentRunMessage{},
		artifacts:    map[string][]agentcore.AgentRunArtifact{},
		interactions: map[string][]agentcore.AgentRunInteraction{},
		toolCalls:    map[string][]agentcore.ToolCall{},
	}
}

func key(appID, id string) string {
	return appID + "/" + id
}

func (m *Memory) CreateAgent(_ context.Context, agent *agentcore.Agent) error {
	if agent == nil {
		return fmt.Errorf("agent is required")
	}
	if agent.AppID == "" {
		return fmt.Errorf("app_id is required")
	}
	now := time.Now().UTC()
	if agent.ID == "" {
		agent.ID = id.New("agent")
	}
	if agent.RuntimeKind == "" {
		agent.RuntimeKind = agentcore.RuntimeNativeSDK
	}
	if agent.ApprovalMode == "" {
		agent.ApprovalMode = agentcore.ApprovalModeNever
	}
	if agent.DefaultInvocationMode == "" {
		agent.DefaultInvocationMode = agentcore.InvocationAutonomous
	}
	agent.CreatedAt = now
	agent.UpdatedAt = now

	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(agent.AppID, agent.ID)
	if _, exists := m.agents[k]; exists {
		return fmt.Errorf("agent already exists")
	}
	cp := *agent
	m.agents[k] = &cp
	return nil
}

func (m *Memory) GetAgent(_ context.Context, appID, agentID string) (*agentcore.Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent := m.agents[key(appID, agentID)]
	if agent == nil {
		return nil, nil
	}
	cp := *agent
	return &cp, nil
}

func (m *Memory) ListAgents(_ context.Context, appID string) ([]agentcore.Agent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]agentcore.Agent, 0)
	for _, agent := range m.agents {
		if agent.AppID == appID {
			out = append(out, *agent)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) UpdateAgent(_ context.Context, agent *agentcore.Agent) error {
	if agent == nil {
		return fmt.Errorf("agent is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(agent.AppID, agent.ID)
	existing := m.agents[k]
	if existing == nil {
		return fmt.Errorf("agent not found")
	}
	agent.CreatedAt = existing.CreatedAt
	agent.UpdatedAt = time.Now().UTC()
	cp := *agent
	m.agents[k] = &cp
	return nil
}

func (m *Memory) CreateRun(_ context.Context, run *agentcore.AgentRun) error {
	if run == nil {
		return fmt.Errorf("run is required")
	}
	if run.AppID == "" {
		return fmt.Errorf("app_id is required")
	}
	now := time.Now().UTC()
	if run.ID == "" {
		run.ID = id.New("run")
	}
	run.CreatedAt = now
	run.UpdatedAt = now
	agentcore.NormalizeRun(run)

	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(run.AppID, run.ID)
	if _, exists := m.runs[k]; exists {
		return fmt.Errorf("run already exists")
	}
	cp := *run
	m.runs[k] = &cp
	return nil
}

func (m *Memory) GetRun(_ context.Context, appID, runID string) (*agentcore.AgentRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	run := m.runs[key(appID, runID)]
	if run == nil {
		return nil, nil
	}
	cp := *run
	return &cp, nil
}

func (m *Memory) ListRuns(_ context.Context, appID string) ([]agentcore.AgentRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]agentcore.AgentRun, 0)
	for _, run := range m.runs {
		if run.AppID == appID {
			out = append(out, *run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) UpdateRun(_ context.Context, run *agentcore.AgentRun) error {
	if run == nil {
		return fmt.Errorf("run is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(run.AppID, run.ID)
	existing := m.runs[k]
	if existing == nil {
		return fmt.Errorf("run not found")
	}
	run.CreatedAt = existing.CreatedAt
	run.UpdatedAt = time.Now().UTC()
	cp := *run
	m.runs[k] = &cp
	return nil
}

func (m *Memory) AppendMessage(_ context.Context, message *agentcore.AgentRunMessage) error {
	if message == nil {
		return fmt.Errorf("message is required")
	}
	now := time.Now().UTC()
	if message.ID == "" {
		message.ID = id.New("msg")
	}
	message.CreatedAt = now
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(message.AppID, message.RunID)
	message.SequenceNo = len(m.messages[k]) + 1
	m.messages[k] = append(m.messages[k], *message)
	return nil
}

func (m *Memory) ListMessages(_ context.Context, appID, runID string) ([]agentcore.AgentRunMessage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := m.messages[key(appID, runID)]
	out := append([]agentcore.AgentRunMessage(nil), items...)
	return out, nil
}

func (m *Memory) AppendArtifact(_ context.Context, artifact *agentcore.AgentRunArtifact) error {
	if artifact == nil {
		return fmt.Errorf("artifact is required")
	}
	now := time.Now().UTC()
	if artifact.ID == "" {
		artifact.ID = id.New("art")
	}
	if artifact.StorageMode == "" {
		artifact.StorageMode = "inline"
	}
	artifact.CreatedAt = now
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(artifact.AppID, artifact.RunID)
	artifact.SequenceNo = len(m.artifacts[k]) + 1
	m.artifacts[k] = append(m.artifacts[k], *artifact)
	return nil
}

func (m *Memory) ListArtifacts(_ context.Context, appID, runID string) ([]agentcore.AgentRunArtifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := m.artifacts[key(appID, runID)]
	return append([]agentcore.AgentRunArtifact(nil), items...), nil
}

func (m *Memory) AppendInteraction(_ context.Context, interaction *agentcore.AgentRunInteraction) error {
	if interaction == nil {
		return fmt.Errorf("interaction is required")
	}
	now := time.Now().UTC()
	if interaction.ID == "" {
		interaction.ID = id.New("int")
	}
	if interaction.Status == "" {
		interaction.Status = "pending"
	}
	interaction.CreatedAt = now
	interaction.UpdatedAt = now
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(interaction.AppID, interaction.RunID)
	m.interactions[k] = append(m.interactions[k], *interaction)
	return nil
}

func (m *Memory) ListInteractions(_ context.Context, appID, runID string) ([]agentcore.AgentRunInteraction, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := m.interactions[key(appID, runID)]
	return append([]agentcore.AgentRunInteraction(nil), items...), nil
}

func (m *Memory) UpdateInteraction(_ context.Context, interaction *agentcore.AgentRunInteraction) error {
	if interaction == nil {
		return fmt.Errorf("interaction is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(interaction.AppID, interaction.RunID)
	items := m.interactions[k]
	for i := range items {
		if items[i].ID == interaction.ID {
			interaction.CreatedAt = items[i].CreatedAt
			interaction.UpdatedAt = time.Now().UTC()
			items[i] = *interaction
			m.interactions[k] = items
			return nil
		}
	}
	return fmt.Errorf("interaction not found")
}

func (m *Memory) AppendToolCall(_ context.Context, call *agentcore.ToolCall) error {
	if call == nil {
		return fmt.Errorf("tool call is required")
	}
	if call.ID == "" {
		call.ID = id.New("toolcall")
	}
	call.CreatedAt = time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(call.AppID, call.RunID)
	m.toolCalls[k] = append(m.toolCalls[k], *call)
	return nil
}

func (m *Memory) ListToolCalls(_ context.Context, appID, runID string) ([]agentcore.ToolCall, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := m.toolCalls[key(appID, runID)]
	return append([]agentcore.ToolCall(nil), items...), nil
}
