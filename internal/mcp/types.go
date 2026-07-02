package mcp

import (
	"encoding/json"

	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/sdk"
)

type Tool = sdk.Tool
type ContentItem = sdk.ContentItem
type CallResult = sdk.ToolCallResult
type ProviderToolCallRequest = sdk.ProviderToolCallRequest

type ToolProvider interface {
	ListTools() ([]Tool, error)
	CallTool(name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error)
}
