package mcp

import (
	"encoding/json"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type Tool struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description"`
	Category             string          `json:"category,omitempty"`
	InputSchema          json.RawMessage `json:"input_schema"`
	Mutating             bool            `json:"mutating,omitempty"`
	SupportedTargetTypes []string        `json:"supported_target_types,omitempty"`
}

type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type CallResult struct {
	Content          []ContentItem `json:"content"`
	IsError          bool          `json:"is_error,omitempty"`
	ApprovalRequired bool          `json:"approval_required,omitempty"`
	InteractionID    string        `json:"interaction_id,omitempty"`
}

type ProviderToolCallRequest struct {
	ToolName string                        `json:"tool_name"`
	Input    json.RawMessage               `json:"input"`
	Meta     tools.CommandExecutionContext `json:"meta"`
}

type ToolProvider interface {
	ListTools() ([]Tool, error)
	CallTool(name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error)
}
