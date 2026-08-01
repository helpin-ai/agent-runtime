package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

var codexNativeRuntimeTools = map[string]bool{
	"request_user_input": true,
	"update_plan":        true,
}

func codexDynamicToolSpecs(ctx context.Context, execCtx *ExecutionContext) ([]codexDynamicToolSpec, error) {
	if execCtx == nil || execCtx.Store == nil || execCtx.Tools == nil || execCtx.Run == nil {
		return nil, nil
	}
	listed, err := mcp.NewGateway(execCtx.Store, execCtx.Tools).ListTools(ctx, execCtx.Run.AppID, execCtx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("list Codex dynamic tools: %w", err)
	}
	result := make([]codexDynamicToolSpec, 0, len(listed))
	for _, tool := range listed {
		name := tools.CanonicalName(tool.Name)
		if name == "" || codexNativeRuntimeTools[name] {
			continue
		}
		schema := normalizeCodexDynamicToolSchema(tool.InputSchema)
		result = append(result, codexDynamicToolSpec{
			Type:        "function",
			Name:        name,
			Description: strings.TrimSpace(tool.Description),
			InputSchema: schema,
		})
	}
	return result, nil
}

func normalizeCodexDynamicToolSchema(schema json.RawMessage) json.RawMessage {
	fallback := json.RawMessage(`{"type":"object","properties":{}}`)
	if len(schema) == 0 {
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

func handleCodexDynamicToolCall(ctx context.Context, client *codexAppServerClient, msg codexRPCMessage, execCtx *ExecutionContext) error {
	if client == nil {
		return fmt.Errorf("Codex app-server client is required")
	}
	if len(msg.ID) == 0 {
		return fmt.Errorf("Codex item/tool/call request is missing an id")
	}
	var params codexDynamicToolCallParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return respondCodexDynamicToolFailure(ctx, client, msg.ID, fmt.Sprintf("decode dynamic tool call: %v", err))
	}
	toolName := codexDynamicToolLogicalName(params)
	if toolName == "" {
		return respondCodexDynamicToolFailure(ctx, client, msg.ID, "dynamic tool name is required")
	}
	if execCtx == nil || execCtx.Store == nil || execCtx.Tools == nil || execCtx.Run == nil {
		return respondCodexDynamicToolFailure(ctx, client, msg.ID, "Agent Runtime tool gateway is not configured")
	}
	arguments := params.Arguments
	if len(arguments) == 0 || strings.TrimSpace(string(arguments)) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	result, err := mcp.NewGateway(execCtx.Store, execCtx.Tools).CallTool(ctx, execCtx.Run.AppID, execCtx.Run.ID, mcp.ToolCallRequest{
		ToolName: toolName,
		Input:    arguments,
	})
	if err != nil {
		return respondCodexDynamicToolFailure(ctx, client, msg.ID, err.Error())
	}
	response := codexDynamicToolCallResponse{Success: result != nil && !result.IsError}
	if result != nil {
		for _, item := range result.Content {
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
			response.ContentItems = append(response.ContentItems, codexDynamicToolCallOutputContentItem{
				Type: "inputText",
				Text: item.Text,
			})
		}
	}
	if len(response.ContentItems) == 0 {
		text := "{}"
		if !response.Success {
			text = "dynamic tool call failed"
		}
		response.ContentItems = []codexDynamicToolCallOutputContentItem{{Type: "inputText", Text: text}}
	}
	return client.Respond(ctx, msg.ID, response)
}

func codexDynamicToolLogicalName(params codexDynamicToolCallParams) string {
	toolName := tools.CanonicalName(params.Tool)
	if params.Namespace == nil || strings.TrimSpace(*params.Namespace) == "" {
		return toolName
	}
	return tools.CanonicalName(strings.TrimSpace(*params.Namespace) + "__" + strings.TrimSpace(params.Tool))
}

func respondCodexDynamicToolFailure(ctx context.Context, client *codexAppServerClient, id json.RawMessage, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "dynamic tool call failed"
	}
	return client.Respond(ctx, id, codexDynamicToolCallResponse{
		ContentItems: []codexDynamicToolCallOutputContentItem{{Type: "inputText", Text: message}},
		Success:      false,
	})
}
