package tools

import (
	"fmt"
	"strings"

	sdk "github.com/helpin-ai/agent-runtime-go"
)

type CommandExecutionContext = sdk.CommandExecutionContext

func CommandExecutionContextFromCallContext(callCtx CallContext) CommandExecutionContext {
	meta := CommandExecutionContext{
		AppID:  callCtx.AppID,
		RunID:  callCtx.RunID,
		Target: callCtx.Target,
	}
	if callCtx.Agent != nil {
		meta.AgentID = callCtx.Agent.ID
		if meta.AppID == "" {
			meta.AppID = callCtx.Agent.AppID
		}
	}
	if callCtx.Run != nil {
		meta.AppID = firstNonEmptyString(meta.AppID, callCtx.Run.AppID)
		meta.RunID = firstNonEmptyString(meta.RunID, callCtx.Run.ID)
		meta.AgentID = firstNonEmptyString(meta.AgentID, callCtx.Run.AgentID)
		meta.ExternalActorID = callCtx.Run.ExternalActorID
		if meta.Target.Type == "" && meta.Target.ID == "" {
			meta.Target = callCtx.Run.Target
		}
		meta.RunInputMetadata = callCtx.Run.Input.Metadata
		if value := stringFromAnyMap(callCtx.Run.Input.Metadata, "workspace_id"); value != "" {
			meta.WorkspaceID = value
		}
		if callCtx.Run.WorkspaceLease != nil {
			meta.WorkspaceMetadata = callCtx.Run.WorkspaceLease.Metadata
			meta.WorkspaceID = firstNonEmptyString(meta.WorkspaceID, stringFromAnyMap(callCtx.Run.WorkspaceLease.Metadata, "workspace_id"))
		}
	}
	if meta.Target.Type == "" && meta.Target.ID == "" {
		meta.Target = callCtx.Target
	}
	meta.TargetType = meta.Target.Type
	meta.TargetID = meta.Target.ID
	meta.TargetMetadata = meta.Target.Metadata
	meta.WorkspaceID = firstNonEmptyString(meta.WorkspaceID, stringFromAnyMap(meta.Target.Metadata, "workspace_id"))
	return meta
}

func stringFromAnyMap(values map[string]interface{}, key string) string {
	if values == nil {
		return ""
	}
	switch value := values[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		return ""
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
