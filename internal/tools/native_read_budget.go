package tools

import "encoding/json"

func workspaceReadContentBudget(ctx CallContext) int {
	if nativeManagedReadBudget(ctx) {
		return 8192
	}
	return readFilesContentBudget(1)
}

// The larger read window is enabled together with bounded native context only.
// Keep this independent of runtime (which already depends on tools).
func nativeManagedReadBudget(ctx CallContext) bool {
	if ctx.Agent == nil || ctx.Agent.RuntimeKind != "native_sdk" {
		return false
	}
	var config struct {
		Context struct {
			Enabled bool `json:"enabled"`
		} `json:"native_context"`
	}
	return json.Unmarshal(ctx.Agent.ExecutionConfig, &config) == nil && config.Context.Enabled
}
