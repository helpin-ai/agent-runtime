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
	registrations, registered, err := ProviderRegistrations(ctx, provider, prefix)
	if err != nil {
		return nil, err
	}
	for _, registration := range registrations {
		registry.Register(registration.Definition, registration.Handler)
	}
	return registered, nil
}

// ProviderRegistrations discovers and validates a provider catalog without mutating a registry.
func ProviderRegistrations(ctx context.Context, provider ToolProvider, prefix string) ([]tools.ProviderRegistration, []string, error) {
	if provider == nil {
		return nil, nil, fmt.Errorf("mcp tool provider is not configured")
	}
	providerTools, err := provider.ListTools(ctx)
	if err != nil {
		return nil, nil, err
	}
	registrations := make([]tools.ProviderRegistration, 0, len(providerTools))
	registered := make([]string, 0, len(providerTools))
	seen := map[string]bool{}
	for _, providerTool := range providerTools {
		originalName := strings.TrimSpace(providerTool.Name)
		exposedName := exposedToolName(prefix, originalName)
		if exposedName == "" {
			continue
		}
		if seen[exposedName] {
			return nil, nil, fmt.Errorf("MCP provider contains duplicate tool %q", exposedName)
		}
		seen[exposedName] = true
		def := tools.Definition{
			Name:        exposedName,
			Description: strings.TrimSpace(providerTool.Description),
			Category:    providerTool.Category,
			InputSchema: schemaForDefinition(providerTool.InputSchema),
			Mutating:    providerTool.Mutating,
			RiskLevel:   strings.TrimSpace(providerTool.RiskLevel),
		}
		handler := func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
			result, err := provider.CallTool(ctx, originalName, input, tools.CommandExecutionContextFromCallContext(callCtx))
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
			if len(result.StructuredContent) > 0 && string(result.StructuredContent) != "null" {
				return append(json.RawMessage(nil), result.StructuredContent...), nil
			}
			text := strings.TrimSpace(joinContentText(result.Content))
			if text == "" {
				payload, _ := json.Marshal(result)
				return payload, nil
			}
			return json.Marshal(map[string]string{"text": text})
		}
		registrations = append(registrations, tools.ProviderRegistration{Definition: def, Handler: handler})
		registered = append(registered, exposedName)
	}
	return registrations, registered, nil
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
