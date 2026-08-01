package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func codexDynamicToolSpecs(ctx context.Context, execCtx *ExecutionContext) ([]codexDynamicToolSpec, error) {
	if execCtx == nil || len(execCtx.AllowedTools) == 0 {
		return nil, nil
	}
	if execCtx.Store == nil || execCtx.Tools == nil || execCtx.Run == nil {
		return nil, fmt.Errorf("tool-enabled run is missing its store, registry, or run context")
	}
	listed, err := mcp.NewGatewayWithAllowed(execCtx.Store, execCtx.Tools, execCtx.AllowedTools).ListTools(ctx, execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("list Codex dynamic tools: %w", err)
	}
	specs := make([]codexDynamicToolSpec, 0, len(listed))
	for _, tool := range listed {
		name := tools.CanonicalName(tool.Name)
		if name == "" {
			continue
		}
		// Advertise update_plan and request_user_input too. Codex keeps its
		// native handler when a native tool with the same name is present; this
		// definition is the fallback for versions or modes without one.
		specs = append(specs, codexDynamicToolSpec{
			Type:        "function",
			Name:        name,
			Description: strings.TrimSpace(tool.Description),
			InputSchema: normalizeCodexDynamicToolSchema(tool.InputSchema),
		})
	}
	return specs, nil
}

func normalizeCodexDynamicToolSchema(schema json.RawMessage) json.RawMessage {
	fallback := json.RawMessage(`{"type":"object","properties":{}}`)
	if len(schema) == 0 || strings.TrimSpace(string(schema)) == "null" {
		return fallback
	}
	var object map[string]any
	if err := json.Unmarshal(schema, &object); err != nil || object == nil {
		return fallback
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return fallback
	}
	return normalized
}

func (a *CodexAdapter) handleCodexDynamicToolCall(ctx context.Context, client codexAppServerRPC, execCtx *ExecutionContext, msg codexRPCMessage) error {
	var params codexDynamicToolCallParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return client.Respond(ctx, msg.ID, codexDynamicToolFailure(fmt.Sprintf("invalid dynamic tool request: %v", err)))
	}
	if execCtx == nil || execCtx.Store == nil || execCtx.Tools == nil || execCtx.Run == nil {
		return client.Respond(ctx, msg.ID, codexDynamicToolFailure("agent runtime tools are not configured for this run"))
	}
	toolName := codexDynamicToolLogicalName(params)
	if toolName == "" {
		return client.Respond(ctx, msg.ID, codexDynamicToolFailure("dynamic tool name is required"))
	}
	if toolName == "request_approval" || toolName == "request_review_checkpoint" {
		missing, err := missingCodexCompletionTools(ctx, execCtx)
		if err != nil {
			return client.Respond(ctx, msg.ID, codexDynamicToolFailure(err.Error()))
		}
		if len(missing) > 0 {
			return client.Respond(ctx, msg.ID, codexDynamicToolFailure("complete the required tool calls before requesting approval: "+strings.Join(missing, ", ")))
		}
	}
	arguments := params.Arguments
	if len(arguments) == 0 || strings.TrimSpace(string(arguments)) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	result, err := mcp.NewGatewayWithAllowed(execCtx.Store, execCtx.Tools, execCtx.AllowedTools).CallTool(ctx, execCtx.AppID, execCtx.Run.ID, mcp.ToolCallRequest{
		ToolName: toolName,
		Input:    arguments,
	})
	if err != nil {
		return client.Respond(ctx, msg.ID, codexDynamicToolFailure(err.Error()))
	}
	if result == nil {
		return client.Respond(ctx, msg.ID, codexDynamicToolFailure("dynamic tool call returned no result"))
	}
	content := make([]codexDynamicToolCallOutputContentItem, 0, len(result.Content))
	for _, item := range result.Content {
		if strings.TrimSpace(item.Text) == "" {
			continue
		}
		content = append(content, codexDynamicToolCallOutputContentItem{Type: "inputText", Text: item.Text})
	}
	if len(content) == 0 {
		content = append(content, codexDynamicToolCallOutputContentItem{Type: "inputText", Text: "{}"})
	}
	return client.Respond(ctx, msg.ID, codexDynamicToolCallResponse{
		Success:      !result.IsError && !result.ApprovalRequired,
		ContentItems: content,
	})
}

func codexDynamicToolLogicalName(params codexDynamicToolCallParams) string {
	toolName := tools.CanonicalName(params.Tool)
	if params.Namespace == nil || strings.TrimSpace(*params.Namespace) == "" {
		return toolName
	}
	return tools.CanonicalName(strings.TrimSpace(*params.Namespace) + "__" + strings.TrimSpace(params.Tool))
}

func codexDynamicToolFailure(message string) codexDynamicToolCallResponse {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "dynamic tool call failed"
	}
	return codexDynamicToolCallResponse{
		Success: false,
		ContentItems: []codexDynamicToolCallOutputContentItem{{
			Type: "inputText",
			Text: message,
		}},
	}
}

func interruptCodexTurnAfterDynamicInteraction(ctx context.Context, client codexAppServerRPC, msg codexRPCMessage) error {
	var params codexDynamicToolCallParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return fmt.Errorf("parse Codex interaction tool context: %w", err)
	}
	threadID := strings.TrimSpace(params.ThreadID)
	turnID := strings.TrimSpace(params.TurnID)
	if threadID == "" || turnID == "" {
		return fmt.Errorf("Codex interaction tool call is missing thread or turn context")
	}
	if _, err := client.Request(ctx, "turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	}); err != nil {
		return fmt.Errorf("interrupt Codex turn after interaction request: %w", err)
	}
	return nil
}
