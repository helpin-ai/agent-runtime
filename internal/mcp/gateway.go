package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type Gateway struct {
	store       agentcore.Store
	tools       *tools.Registry
	allowed     map[string]bool
	callContext tools.CallContext
}

// NewGatewayWithAllowed uses an execution-scoped allowlist. It is used for
// isolated run MCP overlays whose aliases do not belong to the saved agent.
func NewGatewayWithAllowed(store agentcore.Store, registry *tools.Registry, allowed map[string]bool) *Gateway {
	copyAllowed := make(map[string]bool, len(allowed))
	for name, value := range allowed {
		if value {
			copyAllowed[tools.CanonicalName(name)] = true
		}
	}
	return &Gateway{store: store, tools: registry, allowed: copyAllowed}
}

func NewGateway(store agentcore.Store, registry *tools.Registry) *Gateway {
	return &Gateway{store: store, tools: registry}
}

// WithCallContext adds execution-local resources, such as the staged skill
// root, without changing the persisted run or the public MCP contract.
func (g *Gateway) WithCallContext(callContext tools.CallContext) *Gateway {
	if g != nil {
		g.callContext = callContext
	}
	return g
}

type ToolCallRequest struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

func (g *Gateway) ListTools(ctx context.Context, appID, runID string) ([]Tool, error) {
	state, err := g.resolveRunToolState(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	allowed := g.effectiveTools(state.run, state.agent)
	out := make([]Tool, 0)
	for _, def := range g.tools.DefinitionsForApp(appID) {
		if !allowed[def.Name] {
			continue
		}
		if err := validateTarget(def, state.run.Target.Type); err != nil {
			continue
		}
		out = append(out, toolFromDefinition(def))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (g *Gateway) CallTool(ctx context.Context, appID, runID string, req ToolCallRequest) (*CallResult, error) {
	state, err := g.resolveRunToolState(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	toolName := tools.CanonicalName(req.ToolName)
	if toolName == "" {
		return nil, fmt.Errorf("tool_name is required")
	}
	if !g.effectiveTools(state.run, state.agent)[toolName] {
		return nil, fmt.Errorf("tool %q is not allowed for this run", toolName)
	}
	def, ok := g.tools.DefinitionForApp(appID, toolName)
	if !ok {
		return nil, fmt.Errorf("tool %q is not registered", toolName)
	}
	if err := validateTarget(def, state.run.Target.Type); err != nil {
		return nil, err
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage(`{}`)
	}
	if def.Mutating && requiresApproval(state.agent, state.run, def) {
		interactionID, err := g.createToolApprovalInteraction(ctx, state.run, def, req.Input)
		if err != nil {
			return nil, err
		}
		resp := &CallResult{
			Content: []ContentItem{{
				Type: "text",
				Text: fmt.Sprintf("approval_required: approval is required before running %s", toolName),
			}},
			ApprovalRequired: true,
			InteractionID:    interactionID,
		}
		_ = g.recordToolCall(ctx, state.run, toolName, req.Input, resp, nil, true, def.Mutating)
		return resp, nil
	}
	approvalMode := state.agent.ApprovalMode
	perToolApproval := approvalMode == agentcore.ApprovalModeMutatingTools || approvalMode == agentcore.ApprovalModeRiskBased
	if def.Mutating && perToolApproval && state.run.ApprovalState == agentcore.ApprovalApproved {
		// Per-tool approvals authorize one attempted mutation. Consume the
		// approval before invoking the side effect so a later tool call gates on
		// its own interaction.
		state.run.ApprovalState = agentcore.ApprovalNotRequired
		if err := g.store.UpdateRun(ctx, state.run); err != nil {
			return nil, fmt.Errorf("consume tool approval: %w", err)
		}
	}

	callContext := g.callContext
	callContext.AppID = state.run.AppID
	callContext.RunID = state.run.ID
	callContext.Agent = state.agent
	callContext.Run = state.run
	callContext.Target = state.run.Target
	if callContext.ArtifactWriter == nil {
		callContext.ArtifactWriter = gatewayArtifactWriter{store: g.store, run: state.run}
	}
	output, err := g.tools.Execute(ctx, callContext, toolName, req.Input)
	resp := &CallResult{}
	if err != nil {
		resp.IsError = true
		resp.Content = []ContentItem{{Type: "text", Text: err.Error()}}
		_ = g.recordToolCall(ctx, state.run, toolName, req.Input, resp, err, false, def.Mutating)
		return resp, nil
	}
	text := strings.TrimSpace(tools.ToolResultText(output))
	if text == "" {
		text = "{}"
	}
	resp.Content = []ContentItem{{Type: "text", Text: text}}
	_ = g.recordToolCall(ctx, state.run, toolName, req.Input, resp, nil, false, def.Mutating)
	return resp, nil
}

func (g *Gateway) effectiveTools(run *agentcore.AgentRun, agent *agentcore.Agent) map[string]bool {
	if g != nil && g.allowed != nil {
		return g.allowed
	}
	return effectiveTools(run, agent)
}

type runToolState struct {
	run   *agentcore.AgentRun
	agent *agentcore.Agent
}

func (g *Gateway) resolveRunToolState(ctx context.Context, appID, runID string) (*runToolState, error) {
	if g == nil || g.store == nil || g.tools == nil {
		return nil, fmt.Errorf("mcp gateway is not configured")
	}
	appID = strings.TrimSpace(appID)
	runID = strings.TrimSpace(runID)
	if appID == "" || runID == "" {
		return nil, fmt.Errorf("app_id and run_id are required")
	}
	run, err := g.store.GetRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("agent run not found")
	}
	if agentcore.IsTerminalStatus(run.Status) {
		return nil, fmt.Errorf("agent run is not active")
	}
	agent, err := g.store.GetAgent(ctx, run.AppID, run.AgentID)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, fmt.Errorf("agent not found")
	}
	return &runToolState{run: run, agent: agent}, nil
}

func effectiveTools(run *agentcore.AgentRun, agent *agentcore.Agent) map[string]bool {
	agentTools := make([]string, 0)
	if agent != nil {
		for _, tool := range agent.AllowedTools {
			if tool = tools.CanonicalName(tool); tool != "" {
				agentTools = append(agentTools, tool)
			}
		}
	}
	selected := agentTools
	if run != nil && len(run.Input.AllowedTools) > 0 {
		allowed := make(map[string]bool, len(agentTools))
		for _, tool := range agentTools {
			allowed[tool] = true
		}
		selected = make([]string, 0, len(run.Input.AllowedTools))
		for _, tool := range run.Input.AllowedTools {
			tool = tools.CanonicalName(tool)
			if allowed[tool] {
				selected = append(selected, tool)
			}
		}
	}
	out := make(map[string]bool, len(selected))
	for _, tool := range selected {
		if tool != "" {
			out[tool] = true
		}
	}
	return out
}

func toolFromDefinition(def tools.Definition) Tool {
	schema, _ := json.Marshal(def.InputSchema)
	if len(schema) == 0 || string(schema) == "null" {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return Tool{
		Name:                 def.Name,
		Description:          def.Description,
		Category:             def.Category,
		InputSchema:          schema,
		Mutating:             def.Mutating,
		SupportedTargetTypes: append([]string(nil), def.SupportedTargetTypes...),
	}
}

func validateTarget(def tools.Definition, targetType string) error {
	if len(def.SupportedTargetTypes) == 0 || strings.TrimSpace(targetType) == "" {
		return nil
	}
	for _, supported := range def.SupportedTargetTypes {
		if supported == targetType {
			return nil
		}
	}
	return fmt.Errorf("tool %q does not support target type %q", def.Name, targetType)
}

func requiresApproval(agent *agentcore.Agent, run *agentcore.AgentRun, def tools.Definition) bool {
	if agent == nil {
		return true
	}
	if run != nil && run.ApprovalState == agentcore.ApprovalApproved {
		return false
	}
	switch strings.TrimSpace(agent.ApprovalMode) {
	case "", agentcore.ApprovalModeNever:
		return false
	case agentcore.ApprovalModeRiskBased:
		return def.EffectiveRiskLevel() == tools.RiskLevelSensitive || def.EffectiveRiskLevel() == tools.RiskLevelDestructive
	default:
		return true
	}
}

func (g *Gateway) createToolApprovalInteraction(ctx context.Context, run *agentcore.AgentRun, def tools.Definition, input json.RawMessage) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"tool_name":  def.Name,
		"mutating":   def.Mutating,
		"risk_level": def.EffectiveRiskLevel(),
		"input":      json.RawMessage(input),
	})
	interaction := &agentcore.AgentRunInteraction{
		AppID:           run.AppID,
		RunID:           run.ID,
		RuntimeKind:     run.RuntimeKind,
		InteractionKind: "approval_request",
		Status:          "pending",
		Title:           "Approve tool call",
		Summary:         fmt.Sprintf("Approve %s for this agent run.", def.Name),
		RequestPayload:  payload,
	}
	if err := g.store.AppendInteraction(ctx, interaction); err != nil {
		return "", err
	}
	return interaction.ID, nil
}

func (g *Gateway) recordToolCall(ctx context.Context, run *agentcore.AgentRun, toolName string, input json.RawMessage, resp *CallResult, callErr error, approvalRequired, mutating bool) error {
	output, _ := json.Marshal(resp)
	errMsg := ""
	if callErr != nil {
		errMsg = callErr.Error()
	}
	return g.store.AppendToolCall(ctx, &agentcore.ToolCall{
		AppID:            run.AppID,
		RunID:            run.ID,
		ToolName:         toolName,
		Input:            input,
		Output:           output,
		Error:            errMsg,
		Mutating:         mutating,
		ApprovalRequired: approvalRequired,
		CreatedAt:        time.Now().UTC(),
	})
}

// gatewayArtifactWriter persists tool-produced artifacts for the run that
// invoked the tool through the MCP gateway.
type gatewayArtifactWriter struct {
	store agentcore.Store
	run   *agentcore.AgentRun
}

func (w gatewayArtifactWriter) WriteArtifact(ctx context.Context, artifact agentcore.AgentRunArtifact) error {
	if w.store == nil || w.run == nil {
		return fmt.Errorf("artifact writer is not configured")
	}
	artifact.AppID = w.run.AppID
	artifact.RunID = w.run.ID
	return w.store.AppendArtifact(ctx, &artifact)
}
