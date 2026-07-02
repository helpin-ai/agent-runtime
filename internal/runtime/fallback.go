package runtime

import (
	"os"
	"strings"
)

func deterministicFallbackAllowed() bool {
	if truthyRuntimeEnv("AGENT_RUNTIME_ALLOW_DETERMINISTIC_FALLBACK") {
		return true
	}
	if truthyRuntimeEnv("AGENT_RUNTIME_DISABLE_DETERMINISTIC_FALLBACK") {
		return false
	}
	return false
}

func truthyRuntimeEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
