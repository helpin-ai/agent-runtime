package mcp

import (
	"context"
	"encoding/json"

	"github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type Tool = sdk.Tool
type ContentItem = sdk.ContentItem
type CallResult = sdk.ToolCallResult
type ProviderToolCallRequest = sdk.ProviderToolCallRequest

type ToolProvider interface {
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error)
}
