package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

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
		params, err := toEinoJSONSchema(def.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("convert tool %q schema: %w", def.Name, err)
		}
		result = append(result, &schema.ToolInfo{
			Name:        mapper.ModelName(def.Name),
			Desc:        def.Description,
			ParamsOneOf: schema.NewParamsOneOfByJSONSchema(params),
		})
	}
	return result, nil
}

// Preserve the full contract. ParameterInfo cannot represent unions, bounds,
// references, or additionalProperties, and silently weakens model-facing tools.
func toEinoJSONSchema(input any) (*jsonschema.Schema, error) {
	if input == nil {
		input = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	var raw []byte
	var err error
	if data, ok := input.([]byte); ok {
		raw = data
	} else {
		raw, err = json.Marshal(input)
	}
	if err != nil {
		return nil, err
	}
	var result jsonschema.Schema
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
