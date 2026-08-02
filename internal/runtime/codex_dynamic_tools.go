package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type codexDynamicToolSpec struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type codexDynamicToolCallParams struct {
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId"`
	CallID    string          `json:"callId"`
	Tool      string          `json:"tool"`
	Namespace *string         `json:"namespace,omitempty"`
	Arguments json.RawMessage `json:"arguments"`
}

type codexDynamicToolCallOutput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexDynamicToolCallResponse struct {
	Success      bool                         `json:"success"`
	ContentItems []codexDynamicToolCallOutput `json:"contentItems"`
}

// codexDynamicToolPause reports that a dynamic tool call must pause the run
// instead of returning a tool result. The item/tool/call request stays
// unanswered on purpose: resume either answers its replay or falls back to a
// fresh turn, mirroring how built-in Codex pause requests are handled.
type codexDynamicToolPause struct {
	Pending         *codexPendingRequest
	InteractionKind string
	Summary         string
}

func codexDynamicToolSpecs(ctx context.Context, execCtx *ExecutionContext) ([]codexDynamicToolSpec, error) {
	if execCtx == nil || len(execCtx.AllowedTools) == 0 {
		return nil, nil
	}
	if execCtx.Store == nil || execCtx.Tools == nil || execCtx.Run == nil {
		return nil, fmt.Errorf("tool-enabled run is missing its store, registry, or run context")
	}
	listed, err := mcp.NewGatewayWithAllowed(execCtx.Store, execCtx.Tools, execCtx.AllowedTools).ListTools(ctx, execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		return nil, err
	}
	specs := make([]codexDynamicToolSpec, 0, len(listed))
	seen := make(map[string]bool, len(listed))
	for _, tool := range listed {
		schema := append(json.RawMessage(nil), tool.InputSchema...)
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		name := strings.TrimSpace(tool.Name)
		seen[tools.CanonicalName(name)] = true
		specs = append(specs, codexDynamicToolSpec{
			Type:        "function",
			Name:        name,
			Description: strings.TrimSpace(tool.Description),
			InputSchema: schema,
		})
	}
	// Interaction tools (request_user_input, request_approval,
	// request_review_checkpoint, update_plan) live in the runtime, not the
	// gateway registry, so append them here or Codex never sees them even when
	// a skill requires them.
	for _, def := range nativeInteractionToolDefinitions() {
		name := tools.CanonicalName(def.Name)
		if !execCtx.AllowedTools[name] || seen[name] {
			continue
		}
		schema, err := json.Marshal(def.InputSchema)
		if err != nil || len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		specs = append(specs, codexDynamicToolSpec{
			Type:        "function",
			Name:        name,
			Description: strings.TrimSpace(def.Description),
			InputSchema: schema,
		})
	}
	return specs, nil
}

// handleCodexDynamicToolCall executes a dynamic tool call. A nil pause means
// the call was answered (success or failure) and the turn continues. A
// non-nil pause means the request was intentionally left unanswered and the
// caller must persist the pending state and pause the run.
func (a *CodexAdapter) handleCodexDynamicToolCall(ctx context.Context, client codexAppServerRPC, execCtx *ExecutionContext, msg codexRPCMessage) (*codexDynamicToolPause, error) {
	var params codexDynamicToolCallParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, client.Respond(ctx, msg.ID, codexDynamicToolFailure(fmt.Sprintf("invalid dynamic tool request: %v", err)))
	}
	if execCtx == nil || execCtx.Store == nil || execCtx.Run == nil {
		return nil, client.Respond(ctx, msg.ID, codexDynamicToolFailure("agent runtime tools are not configured for this run"))
	}
	toolName := tools.CanonicalName(params.Tool)
	if toolName == "" {
		return nil, client.Respond(ctx, msg.ID, codexDynamicToolFailure("dynamic tool name is required"))
	}
	arguments := params.Arguments
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	if nativeIsInteractionTool(toolName) {
		return a.handleCodexInteractionToolCall(ctx, client, execCtx, msg, params, toolName, arguments)
	}
	if execCtx.Tools == nil {
		return nil, client.Respond(ctx, msg.ID, codexDynamicToolFailure("agent runtime tools are not configured for this run"))
	}
	result, err := mcp.NewGatewayWithAllowed(execCtx.Store, execCtx.Tools, execCtx.AllowedTools).CallTool(ctx, execCtx.AppID, execCtx.Run.ID, mcp.ToolCallRequest{
		ToolName: toolName,
		Input:    arguments,
	})
	if err != nil {
		return nil, client.Respond(ctx, msg.ID, codexDynamicToolFailure(err.Error()))
	}
	if result.ApprovalRequired {
		// The gateway persisted a pending approval_request interaction. Pause
		// the turn on the unanswered tool call so the approved resume can run
		// the tool and hand Codex its real result.
		return &codexDynamicToolPause{
			Pending:         codexDynamicPendingRequest(codexPendingRequestKindDynamicGatewayApproval, msg, params, toolName),
			InteractionKind: "human_approval",
			Summary:         fmt.Sprintf("Approve %s for this agent run.", toolName),
		}, nil
	}
	content := make([]codexDynamicToolCallOutput, 0, len(result.Content))
	for _, item := range result.Content {
		content = append(content, codexDynamicToolCallOutput{Type: "inputText", Text: item.Text})
	}
	if len(content) == 0 {
		content = append(content, codexDynamicToolCallOutput{Type: "inputText", Text: "{}"})
	}
	return nil, client.Respond(ctx, msg.ID, codexDynamicToolCallResponse{
		Success:      !result.IsError,
		ContentItems: content,
	})
}

// handleCodexInteractionToolCall runs the runtime-owned interaction tools for
// Codex. update_plan answers inline; the pausing tools persist their
// interaction (via nativeInteractionToolOutput) and pause the turn.
func (a *CodexAdapter) handleCodexInteractionToolCall(ctx context.Context, client codexAppServerRPC, execCtx *ExecutionContext, msg codexRPCMessage, params codexDynamicToolCallParams, toolName string, arguments json.RawMessage) (*codexDynamicToolPause, error) {
	if !execCtx.AllowedTools[toolName] {
		return nil, client.Respond(ctx, msg.ID, codexDynamicToolFailure(fmt.Sprintf("tool %q is not allowed for this run", toolName)))
	}
	output, pauseReason, _, err := nativeInteractionToolOutput(ctx, execCtx, toolName, normalizeNativeToolInput(arguments))
	if err != nil {
		return nil, client.Respond(ctx, msg.ID, codexDynamicToolFailure(err.Error()))
	}
	if strings.TrimSpace(pauseReason) == "" {
		text := strings.TrimSpace(output)
		if text == "" {
			text = "{}"
		}
		return nil, client.Respond(ctx, msg.ID, codexDynamicToolCallResponse{
			Success:      true,
			ContentItems: []codexDynamicToolCallOutput{{Type: "inputText", Text: text}},
		})
	}
	kind := codexPendingRequestKindDynamicApproval
	interactionKind := "human_approval"
	if toolName == nativeToolRequestUserInput {
		kind = codexPendingRequestKindDynamicInput
		interactionKind = "human_input"
	}
	return &codexDynamicToolPause{
		Pending:         codexDynamicPendingRequest(kind, msg, params, toolName),
		InteractionKind: interactionKind,
		Summary:         codexInteractionToolSummary(toolName, arguments),
	}, nil
}

func codexDynamicPendingRequest(kind string, msg codexRPCMessage, params codexDynamicToolCallParams, toolName string) *codexPendingRequest {
	return &codexPendingRequest{
		Kind:         kind,
		RequestID:    codexRequestIDString(msg.ID),
		RequestIDRaw: append(json.RawMessage(nil), msg.ID...),
		TurnID:       strings.TrimSpace(params.TurnID),
		ItemID:       strings.TrimSpace(params.CallID),
		Tool:         toolName,
		Payload:      append(json.RawMessage(nil), msg.Params...),
	}
}

func codexInteractionToolSummary(toolName string, arguments json.RawMessage) string {
	switch toolName {
	case nativeToolRequestUserInput:
		var req nativeUserInputRequest
		if err := json.Unmarshal(arguments, &req); err == nil && len(req.Questions) > 0 {
			return nativeUserInputSummary(req)
		}
		return "Codex needs input to continue."
	default:
		var req nativeApprovalRequest
		if err := json.Unmarshal(arguments, &req); err == nil {
			if summary := strings.TrimSpace(firstNonEmpty(req.Summary, req.Title)); summary != "" {
				return summary
			}
		}
		return "Codex requested approval before continuing."
	}
}

func codexDynamicToolFailure(message string) codexDynamicToolCallResponse {
	return codexDynamicToolCallResponse{
		Success: false,
		ContentItems: []codexDynamicToolCallOutput{{
			Type: "inputText",
			Text: strings.TrimSpace(message),
		}},
	}
}
