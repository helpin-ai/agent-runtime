package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func toEinoToolInfos(defs []tools.Definition) ([]*schema.ToolInfo, error) {
	return toEinoToolInfosWithNames(defs, newNativeToolNameMapper(defs))
}

func toEinoToolInfosWithNames(defs []tools.Definition, mapper nativeToolNameMapper) ([]*schema.ToolInfo, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	result := make([]*schema.ToolInfo, 0, len(defs))
	for _, def := range defs {
		params, err := toEinoToolParams(def.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("convert tool %q schema: %w", def.Name, err)
		}
		result = append(result, &schema.ToolInfo{
			Name:        mapper.ModelName(def.Name),
			Desc:        def.Description,
			ParamsOneOf: schema.NewParamsOneOfByParams(params),
		})
	}
	return result, nil
}

func toEinoToolParams(inputSchema any) (map[string]*schema.ParameterInfo, error) {
	schemaMap, ok := normalizeJSONSchemaMap(inputSchema)
	if !ok {
		return nil, nil
	}
	props, _ := normalizeJSONSchemaProperties(schemaMap["properties"])
	requiredSet := jsonSchemaRequiredSet(schemaMap)
	params := make(map[string]*schema.ParameterInfo, len(props))
	for name, raw := range props {
		propMap, _ := normalizeJSONSchemaMap(raw)
		param, err := toEinoParameterInfo(propMap)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", name, err)
		}
		param.Required = requiredSet[name]
		params[name] = param
	}
	return params, nil
}

func toEinoParameterInfo(raw map[string]any) (*schema.ParameterInfo, error) {
	typeName, _ := raw["type"].(string)
	info := &schema.ParameterInfo{
		Type: schema.String,
		Desc: stringValue(raw["description"]),
	}
	switch typeName {
	case "object":
		info.Type = schema.Object
		props, _ := normalizeJSONSchemaProperties(raw["properties"])
		if len(props) > 0 {
			requiredSet := jsonSchemaRequiredSet(raw)
			info.SubParams = make(map[string]*schema.ParameterInfo, len(props))
			for name, value := range props {
				childMap, _ := normalizeJSONSchemaMap(value)
				child, err := toEinoParameterInfo(childMap)
				if err != nil {
					return nil, err
				}
				child.Required = requiredSet[name]
				info.SubParams[name] = child
			}
		}
	case "array":
		info.Type = schema.Array
		if itemMap, ok := normalizeJSONSchemaMap(raw["items"]); ok {
			elem, err := toEinoParameterInfo(itemMap)
			if err != nil {
				return nil, err
			}
			info.ElemInfo = elem
		} else {
			info.ElemInfo = &schema.ParameterInfo{Type: schema.String}
		}
	case "integer":
		info.Type = schema.Integer
	case "number":
		info.Type = schema.Number
	case "boolean":
		info.Type = schema.Boolean
	case "null":
		info.Type = schema.Null
	default:
		info.Type = schema.String
	}
	for _, value := range jsonSchemaEnumValues(raw["enum"]) {
		info.Enum = append(info.Enum, value)
	}
	return info, nil
}

func normalizeJSONSchemaMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case json.RawMessage:
		var out map[string]any
		if json.Unmarshal(typed, &out) == nil {
			return out, true
		}
	case []byte:
		var out map[string]any
		if json.Unmarshal(typed, &out) == nil {
			return out, true
		}
	}
	return nil, false
}

func normalizeJSONSchemaProperties(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	default:
		return nil, false
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func jsonSchemaRequiredSet(raw map[string]any) map[string]bool {
	requiredSet := map[string]bool{}
	switch required := raw["required"].(type) {
	case []string:
		for _, name := range required {
			requiredSet[name] = true
		}
	case []any:
		for _, entry := range required {
			if name, ok := entry.(string); ok {
				requiredSet[name] = true
			}
		}
	}
	return requiredSet
}

func jsonSchemaEnumValues(raw any) []string {
	var out []string
	switch values := raw.(type) {
	case []string:
		return append(out, values...)
	case []any:
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
	}
	return out
}
