package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/mcp"
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
	for _, tool := range listed {
		schema := append(json.RawMessage(nil), tool.InputSchema...)
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		specs = append(specs, codexDynamicToolSpec{
			Type:        "function",
			Name:        strings.TrimSpace(tool.Name),
			Description: strings.TrimSpace(tool.Description),
			InputSchema: schema,
		})
	}
	return specs, nil
}

func (a *CodexAdapter) handleCodexDynamicToolCall(ctx context.Context, client codexAppServerRPC, execCtx *ExecutionContext, msg codexRPCMessage) error {
	var params codexDynamicToolCallParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return client.Respond(ctx, msg.ID, codexDynamicToolFailure(fmt.Sprintf("invalid dynamic tool request: %v", err)))
	}
	if execCtx == nil || execCtx.Store == nil || execCtx.Tools == nil || execCtx.Run == nil {
		return client.Respond(ctx, msg.ID, codexDynamicToolFailure("agent runtime tools are not configured for this run"))
	}
	toolName := strings.TrimSpace(params.Tool)
	if toolName == "" {
		return client.Respond(ctx, msg.ID, codexDynamicToolFailure("dynamic tool name is required"))
	}
	arguments := params.Arguments
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	result, err := mcp.NewGatewayWithAllowed(execCtx.Store, execCtx.Tools, execCtx.AllowedTools).CallTool(ctx, execCtx.AppID, execCtx.Run.ID, mcp.ToolCallRequest{
		ToolName: toolName,
		Input:    arguments,
	})
	if err != nil {
		return client.Respond(ctx, msg.ID, codexDynamicToolFailure(err.Error()))
	}
	content := make([]codexDynamicToolCallOutput, 0, len(result.Content))
	for _, item := range result.Content {
		content = append(content, codexDynamicToolCallOutput{Type: "inputText", Text: item.Text})
	}
	if len(content) == 0 {
		content = append(content, codexDynamicToolCallOutput{Type: "inputText", Text: "{}"})
	}
	return client.Respond(ctx, msg.ID, codexDynamicToolCallResponse{
		Success:      !result.IsError && !result.ApprovalRequired,
		ContentItems: content,
	})
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
