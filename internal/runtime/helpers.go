package runtime

import "strings"

func workspaceRoot(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.WorkspaceLease == nil {
		return ""
	}
	return strings.TrimSpace(execCtx.WorkspaceLease.RootPath)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
