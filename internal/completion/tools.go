package completion

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// RequiredTools returns completion tools from the stable agent contract plus
// trusted requirements scoped to this individual run by the host app.
func RequiredTools(agent *agentcore.Agent, run *agentcore.AgentRun) []string {
	var names []string
	if agent != nil && len(agent.ExecutionConfig) > 0 {
		var config struct {
			Completion struct {
				RequiredTools []string `json:"required_tools"`
			} `json:"completion"`
		}
		if json.Unmarshal(agent.ExecutionConfig, &config) == nil {
			names = append(names, config.Completion.RequiredTools...)
		}
	}
	if run != nil {
		names = append(names, metadataStringSlice(run.Input.Metadata, "completion_required_tools")...)
	}

	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		name = tools.CanonicalName(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func metadataStringSlice(metadata map[string]interface{}, key string) []string {
	if len(metadata) == 0 {
		return nil
	}
	switch value := metadata[key].(type) {
	case []string:
		return append([]string(nil), value...)
	case []interface{}:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		if strings.TrimSpace(value) != "" {
			return []string{value}
		}
	}
	return nil
}
