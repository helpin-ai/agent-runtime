package agentcore

import (
	"crypto/sha256"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// TurnIdentity is shared by adapters and lifecycle events. A retry reuses the
// accepted resume ID; a subsequent user reply or interaction gets a new ID.
func TurnIdentity(run *AgentRun) string {
	if run == nil {
		return ""
	}
	resumeID := "initial"
	if resume, ok := run.Input.Metadata["last_resume"].(map[string]interface{}); ok {
		if value, ok := resume["resume_id"].(string); ok && value != "" {
			resumeID = value
		}
	}
	return fmt.Sprintf("turn_%x", sha256.Sum256([]byte(run.AppID+"\x00"+run.ID+"\x00"+resumeID)))
}

// WithTurnEventMetadata is additive; legacy runs retain their old wire shape.
func WithTurnEventMetadata(run *AgentRun, eventType string, data map[string]interface{}) map[string]interface{} {
	if !RequiresExplicitTurnFinish(run) {
		return data
	}
	out := make(map[string]interface{}, len(data)+4)
	for key, value := range data {
		out[key] = value
	}
	out["turn_id"] = TurnIdentity(run)
	out["completion_mode"] = "explicit"
	out["turn_protocol_version"] = 1
	out["runtime_revision"] = RuntimeBuildRevision()
	if start, ok := run.Input.Metadata["turn_started_at"].(string); ok && start != "" {
		out["turn_started_at"] = start
	} else if run.StartedAt != nil {
		out["turn_started_at"] = run.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if strings.HasPrefix(eventType, "assistant_message_") && out["message_type"] == nil {
		out["message_type"] = "assistant_progress"
	}
	return out
}

// RuntimeBuildRevision makes rollout diagnostics explicit even for local builds.
var RuntimeBuildRevision = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "unknown"
})
