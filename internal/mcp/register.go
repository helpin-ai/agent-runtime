package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func RegisterProviderTools(ctx context.Context, registry *tools.Registry, provider ToolProvider, prefix string) ([]string, error) {
	if registry == nil {
		return nil, fmt.Errorf("tool registry is not configured")
	}
	if provider == nil {
		return nil, fmt.Errorf("mcp tool provider is not configured")
	}
	providerTools, err := provider.ListTools()
	if err != nil {
		return nil, err
	}
	registered := make([]string, 0, len(providerTools))
	for _, providerTool := range providerTools {
		originalName := strings.TrimSpace(providerTool.Name)
		exposedName := exposedToolName(prefix, originalName)
		if exposedName == "" {
			continue
		}
		def := tools.Definition{
			Name:        exposedName,
			Description: strings.TrimSpace(providerTool.Description),
			Category:    providerTool.Category,
			InputSchema: schemaForDefinition(providerTool.InputSchema),
			Mutating:    providerTool.Mutating,
		}
		registry.Register(def, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
			result, err := provider.CallTool(originalName, input, tools.CommandExecutionContextFromCallContext(callCtx))
			if err != nil {
				return nil, err
			}
			if result == nil {
				return json.RawMessage(`{}`), nil
			}
			if result.IsError {
				text := strings.TrimSpace(joinContentText(result.Content))
				if text == "" {
					text = "MCP tool returned an error"
				}
				return nil, fmt.Errorf("%s", text)
			}
			text := strings.TrimSpace(joinContentText(result.Content))
			if text == "" {
				payload, _ := json.Marshal(result)
				return payload, nil
			}
			return json.Marshal(map[string]string{"text": text})
		})
		registered = append(registered, exposedName)
	}
	return registered, nil
}

func exposedToolName(prefix, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	prefix = strings.Trim(strings.TrimSpace(prefix), "_")
	if prefix == "" {
		return name
	}
	return prefix + "__" + name
}

func schemaForDefinition(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err == nil && decoded != nil {
		return decoded
	}
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}

func joinContentText(items []ContentItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.Text) != "" {
			parts = append(parts, item.Text)
		}
	}
	return strings.Join(parts, "\n")
}
