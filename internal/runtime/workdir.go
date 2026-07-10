package runtime

import (
	"fmt"
	"os"
	"strings"
)

func resolveRuntimeWorkDir(execCtx *ExecutionContext, configuredWorkDir, runtimeName string) (string, func(), error) {
	if root := workspaceRoot(execCtx); root != "" {
		return root, func() {}, nil
	}
	if configured := strings.TrimSpace(configuredWorkDir); configured != "" {
		return configured, func() {}, nil
	}
	runID := "run"
	if execCtx != nil && execCtx.Run != nil && strings.TrimSpace(execCtx.Run.ID) != "" {
		runID = sanitizeRuntimePathComponent(execCtx.Run.ID)
	}
	name := sanitizeRuntimePathComponent(runtimeName)
	dir, err := os.MkdirTemp("", fmt.Sprintf("agent-runtime-%s-%s-", name, runID))
	if err != nil {
		return "", func() {}, fmt.Errorf("create isolated %s workdir: %w", name, err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func sanitizeRuntimePathComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "run"
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	value = strings.Trim(builder.String(), "-")
	if value == "" {
		return "run"
	}
	return value
}
