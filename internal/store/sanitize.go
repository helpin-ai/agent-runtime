package store

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const postgresJSONReplacement = "\uFFFD"

func sanitizeAgent(agent *agentcore.Agent) {
	if agent == nil {
		return
	}
	agent.Name = sanitizePostgresJSONString(agent.Name)
	agent.SystemPrompt = sanitizePostgresJSONString(agent.SystemPrompt)
	agent.ExecutionConfig = sanitizePostgresJSONRawMessage(agent.ExecutionConfig, nil)
	for i := range agent.Skills {
		agent.Skills[i].SkillID = sanitizePostgresJSONString(agent.Skills[i].SkillID)
		agent.Skills[i].Key = sanitizePostgresJSONString(agent.Skills[i].Key)
		agent.Skills[i].Version = sanitizePostgresJSONString(agent.Skills[i].Version)
		agent.Skills[i].VersionKey = sanitizePostgresJSONString(agent.Skills[i].VersionKey)
		agent.Skills[i].Config = sanitizePostgresJSONRawMessage(agent.Skills[i].Config, nil)
	}
	for i := range agent.AllowedTools {
		agent.AllowedTools[i] = sanitizePostgresJSONString(agent.AllowedTools[i])
	}
	for i := range agent.AllowedTargets {
		agent.AllowedTargets[i] = sanitizePostgresJSONString(agent.AllowedTargets[i])
	}
}

func sanitizeRun(run *agentcore.AgentRun) {
	if run == nil {
		return
	}
	run.Target.Type = sanitizePostgresJSONString(run.Target.Type)
	run.Target.ID = sanitizePostgresJSONString(run.Target.ID)
	if run.Target.Display != nil {
		run.Target.Display.Title = sanitizePostgresJSONString(run.Target.Display.Title)
		run.Target.Display.URL = sanitizePostgresJSONString(run.Target.Display.URL)
	}
	run.Target.Metadata = sanitizeMap(run.Target.Metadata)
	run.HostRunID = sanitizePostgresJSONString(run.HostRunID)
	run.ExternalActorID = sanitizePostgresJSONString(run.ExternalActorID)
	run.Input.Instructions = sanitizePostgresJSONString(run.Input.Instructions)
	run.Input.ContextSummary = sanitizePostgresJSONString(run.Input.ContextSummary)
	for i := range run.Input.AllowedTools {
		run.Input.AllowedTools[i] = sanitizePostgresJSONString(run.Input.AllowedTools[i])
	}
	run.Input.Trigger = sanitizeMap(run.Input.Trigger)
	run.Input.Metadata = sanitizeMap(run.Input.Metadata)
	run.OutputSummary = sanitizePostgresJSONRawMessage(run.OutputSummary, nil)
	if run.WorkspaceLease != nil {
		run.WorkspaceLease.ID = sanitizePostgresJSONString(run.WorkspaceLease.ID)
		run.WorkspaceLease.Provider = sanitizePostgresJSONString(run.WorkspaceLease.Provider)
		run.WorkspaceLease.RootPath = sanitizePostgresJSONString(run.WorkspaceLease.RootPath)
		run.WorkspaceLease.CleanupPolicy = sanitizePostgresJSONString(run.WorkspaceLease.CleanupPolicy)
		run.WorkspaceLease.Metadata = sanitizeMap(run.WorkspaceLease.Metadata)
	}
	run.ErrorMessage = sanitizePostgresJSONString(run.ErrorMessage)
}

func sanitizeMessage(message *agentcore.AgentRunMessage) {
	if message == nil {
		return
	}
	message.RuntimeMessageID = sanitizePostgresJSONString(message.RuntimeMessageID)
	message.Role = sanitizePostgresJSONString(message.Role)
	message.Content = sanitizePostgresJSONString(message.Content)
	message.MessageType = sanitizePostgresJSONString(message.MessageType)
	message.ContentBlocks = sanitizePostgresJSONRawMessage(message.ContentBlocks, nil)
	message.ToolInvocations = sanitizePostgresJSONRawMessage(message.ToolInvocations, nil)
}

func sanitizeArtifact(artifact *agentcore.AgentRunArtifact) {
	if artifact == nil {
		return
	}
	artifact.ArtifactType = sanitizePostgresJSONString(artifact.ArtifactType)
	artifact.Format = sanitizePostgresJSONString(artifact.Format)
	artifact.StorageMode = sanitizePostgresJSONString(artifact.StorageMode)
	artifact.InlineContent = sanitizePostgresJSONString(artifact.InlineContent)
	artifact.Metadata = sanitizePostgresJSONRawMessage(artifact.Metadata, json.RawMessage(`{}`))
}

func sanitizeInteraction(interaction *agentcore.AgentRunInteraction) {
	if interaction == nil {
		return
	}
	interaction.RuntimeKind = sanitizePostgresJSONString(interaction.RuntimeKind)
	interaction.InteractionKind = sanitizePostgresJSONString(interaction.InteractionKind)
	interaction.Status = sanitizePostgresJSONString(interaction.Status)
	interaction.Title = sanitizePostgresJSONString(interaction.Title)
	interaction.Summary = sanitizePostgresJSONString(interaction.Summary)
	interaction.RequestPayload = sanitizePostgresJSONRawMessage(interaction.RequestPayload, json.RawMessage(`{}`))
	interaction.ResponsePayload = sanitizePostgresJSONRawMessage(interaction.ResponsePayload, nil)
	interaction.ResolvedByExternalID = sanitizePostgresJSONString(interaction.ResolvedByExternalID)
}

func sanitizeToolCall(call *agentcore.ToolCall) {
	if call == nil {
		return
	}
	call.ToolName = sanitizePostgresJSONString(call.ToolName)
	call.Input = sanitizePostgresJSONRawMessage(call.Input, json.RawMessage(`{}`))
	call.Output = sanitizePostgresJSONRawMessage(call.Output, nil)
	call.Error = sanitizePostgresJSONString(call.Error)
}

func sanitizeMap(value map[string]interface{}) map[string]interface{} {
	if len(value) == 0 {
		return value
	}
	sanitized := make(map[string]interface{}, len(value))
	for key, item := range value {
		sanitized[sanitizePostgresJSONString(key)] = sanitizePostgresJSONValue(item)
	}
	return sanitized
}

func sanitizePostgresJSONString(value string) string {
	if value == "" {
		return value
	}

	value = strings.ToValidUTF8(value, postgresJSONReplacement)
	if !strings.ContainsRune(value, '\x00') {
		return value
	}
	return strings.ReplaceAll(value, "\x00", postgresJSONReplacement)
}

func sanitizePostgresJSONRawMessage(raw json.RawMessage, fallback json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return fallback
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fallback
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fallback
	}

	sanitized, err := json.Marshal(sanitizePostgresJSONValue(value))
	if err != nil {
		return fallback
	}
	return sanitized
}

func sanitizePostgresJSONValue(value any) any {
	switch typed := value.(type) {
	case string:
		return sanitizePostgresJSONString(typed)
	case []any:
		for i := range typed {
			typed[i] = sanitizePostgresJSONValue(typed[i])
		}
		return typed
	case map[string]any:
		sanitized := make(map[string]any, len(typed))
		for key, value := range typed {
			sanitized[sanitizePostgresJSONString(key)] = sanitizePostgresJSONValue(value)
		}
		return sanitized
	default:
		return typed
	}
}
