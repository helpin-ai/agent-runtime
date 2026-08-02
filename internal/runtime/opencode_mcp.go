package runtime

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helpin-ai/agent-runtime/internal/mcp"
)

type openCodeMCPBroker struct {
	URL    string
	Token  string
	server *http.Server
	listen net.Listener
}

func startOpenCodeMCPBroker(ctx context.Context, execCtx *ExecutionContext) (*openCodeMCPBroker, error) {
	if execCtx == nil || execCtx.Store == nil || execCtx.Tools == nil || execCtx.Run == nil || len(execCtx.AllowedTools) == 0 {
		return nil, nil
	}
	hasRunMCP := false
	for name := range execCtx.AllowedTools {
		if strings.HasPrefix(name, "mcp__") {
			hasRunMCP = true
			break
		}
	}
	if !hasRunMCP {
		return nil, nil
	}
	gateway := mcp.NewGatewayWithAllowed(execCtx.Store, execCtx.Tools, execCtx.AllowedTools)
	definitions, err := gateway.ListTools(ctx, execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		return nil, err
	}
	if len(definitions) == 0 {
		return nil, nil
	}
	server := protocol.NewServer(&protocol.Implementation{Name: "agent-runtime-run-tools", Version: "0.3.0"}, nil)
	registered := 0
	for _, definition := range definitions {
		if !strings.HasPrefix(definition.Name, "mcp__") {
			continue
		}
		registered++
		definition := definition
		var schema any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil || schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
		}
		server.AddTool(&protocol.Tool{Name: definition.Name, Description: definition.Description, InputSchema: schema}, func(callCtx context.Context, request *protocol.CallToolRequest) (*protocol.CallToolResult, error) {
			input := request.Params.Arguments
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			result, err := gateway.CallTool(callCtx, execCtx.AppID, execCtx.Run.ID, mcp.ToolCallRequest{ToolName: definition.Name, Input: input})
			if err != nil {
				return &protocol.CallToolResult{Content: []protocol.Content{&protocol.TextContent{Text: err.Error()}}, IsError: true}, nil
			}
			content := make([]protocol.Content, 0, len(result.Content))
			for _, item := range result.Content {
				content = append(content, &protocol.TextContent{Text: item.Text})
			}
			return &protocol.CallToolResult{Content: content, IsError: result.IsError || result.ApprovalRequired}, nil
		})
	}
	if registered == 0 {
		return nil, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for OpenCode MCP broker: %w", err)
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		_ = listener.Close()
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	mcpHandler := protocol.NewStreamableHTTPHandler(func(*http.Request) *protocol.Server { return server }, &protocol.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		provided := strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, req)
	})
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	broker := &openCodeMCPBroker{URL: "http://" + listener.Addr().String(), Token: token, server: httpServer, listen: listener}
	go func() { _ = httpServer.Serve(listener) }()
	return broker, nil
}

func (b *openCodeMCPBroker) Close() {
	if b == nil || b.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.server.Shutdown(ctx)
}
