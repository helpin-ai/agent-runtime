package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/helpin-ai/agent-runtime/internal/procenv"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcToolList struct {
	Tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	} `json:"tools"`
}

type rpcToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError"`
}

type StreamableHTTPProvider struct {
	Endpoint string
	Token    string
	Client   *http.Client
	nextID   atomic.Int64
}

func (p *StreamableHTTPProvider) ListTools() ([]Tool, error) {
	var out rpcToolList
	if err := p.request(context.Background(), "tools/list", nil, &out); err != nil {
		return nil, err
	}
	return rpcTools(out), nil
}

func (p *StreamableHTTPProvider) CallTool(name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var out rpcToolResult
	if err := p.request(context.Background(), "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": json.RawMessage(input),
	}, &out); err != nil {
		return nil, err
	}
	return &CallResult{Content: out.Content, IsError: out.IsError}, nil
}

func (p *StreamableHTTPProvider) request(ctx context.Context, method string, params interface{}, out interface{}) error {
	endpoint := strings.TrimSpace(p.Endpoint)
	if endpoint == "" {
		return fmt.Errorf("mcp endpoint is required")
	}
	id := p.nextID.Add(1)
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token := strings.TrimSpace(p.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mcp %s failed: %s", method, strings.TrimSpace(string(body)))
	}
	rpcResp, err := decodeRPCResponse(body)
	if err != nil {
		return err
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("mcp %s failed: %s", method, rpcResp.Error.Message)
	}
	if out != nil && len(rpcResp.Result) > 0 {
		return json.Unmarshal(rpcResp.Result, out)
	}
	return nil
}

func (p *StreamableHTTPProvider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: defaultHTTPProviderTimeout}
}

type StdioProvider struct {
	Command string
	Args    []string
	Env     map[string]string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	nextID atomic.Int64
}

func (p *StdioProvider) ListTools() ([]Tool, error) {
	var out rpcToolList
	if err := p.request("tools/list", nil, &out); err != nil {
		return nil, err
	}
	return rpcTools(out), nil
}

func (p *StdioProvider) CallTool(name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var out rpcToolResult
	if err := p.request("tools/call", map[string]interface{}{
		"name":      name,
		"arguments": json.RawMessage(input),
	}, &out); err != nil {
		return nil, err
	}
	return &CallResult{Content: out.Content, IsError: out.IsError}, nil
}

func (p *StdioProvider) request(method string, params interface{}, out interface{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ensureStarted(); err != nil {
		return err
	}
	id := p.nextID.Add(1)
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(p.stdin, "Content-Length: %d\r\n\r\n%s", len(payload), payload); err != nil {
		return err
	}
	resp, err := p.readResponse()
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("mcp %s failed: %s", method, resp.Error.Message)
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

func (p *StdioProvider) ensureStarted() error {
	if p.cmd != nil {
		return nil
	}
	command := strings.TrimSpace(p.Command)
	if command == "" {
		return fmt.Errorf("mcp stdio command is required")
	}
	cmd := exec.Command(command, p.Args...)
	overrides := make([]string, 0, len(p.Env))
	for key, value := range p.Env {
		overrides = append(overrides, key+"="+value)
	}
	cmd.Env = procenv.Sanitized(overrides...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
	p.cmd = cmd
	p.stdin = stdin
	p.reader = bufio.NewReader(stdout)
	if err := p.initialize(); err != nil {
		_ = p.Close()
		return err
	}
	return nil
}

func (p *StdioProvider) initialize() error {
	id := p.nextID.Add(1)
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "initialize",
		Params: map[string]interface{}{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]interface{}{
				"name":    "agent-runtime",
				"version": "0.1.0",
			},
		},
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(p.stdin, "Content-Length: %d\r\n\r\n%s", len(payload), payload); err != nil {
		return err
	}
	resp, err := p.readResponse()
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("mcp initialize failed: %s", resp.Error.Message)
	}
	return nil
}

func (p *StdioProvider) readResponse() (*rpcResponse, error) {
	headers := textproto.MIMEHeader{}
	for {
		line, err := p.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("invalid MCP header %q", line)
		}
		headers.Add(strings.TrimSpace(key), strings.TrimSpace(value))
	}
	length, err := strconv.Atoi(headers.Get("Content-Length"))
	if err != nil || length <= 0 {
		return nil, fmt.Errorf("invalid MCP content length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(p.reader, body); err != nil {
		return nil, err
	}
	return decodeRPCResponse(body)
}

func (p *StdioProvider) Close() error {
	if p == nil {
		return nil
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
	p.cmd = nil
	return nil
}

type FilteringProvider struct {
	Provider ToolProvider
	Allowed  map[string]bool
}

func (p FilteringProvider) ListTools() ([]Tool, error) {
	tools, err := p.Provider.ListTools()
	if err != nil {
		return nil, err
	}
	if len(p.Allowed) == 0 {
		return tools, nil
	}
	out := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if p.Allowed[strings.TrimSpace(tool.Name)] {
			out = append(out, tool)
		}
	}
	return out, nil
}

func (p FilteringProvider) CallTool(name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error) {
	if len(p.Allowed) > 0 && !p.Allowed[strings.TrimSpace(name)] {
		return nil, fmt.Errorf("mcp tool %q is not allowed", name)
	}
	return p.Provider.CallTool(name, input, meta)
}

func rpcTools(list rpcToolList) []Tool {
	tools := make([]Tool, 0, len(list.Tools))
	for _, tool := range list.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, Tool{Name: name, Description: strings.TrimSpace(tool.Description), InputSchema: schema})
	}
	return tools
}

func decodeRPCResponse(body []byte) (*rpcResponse, error) {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				trimmed = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				break
			}
		}
	}
	var resp rpcResponse
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
