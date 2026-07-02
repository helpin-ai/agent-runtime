package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const defaultHTTPProviderTimeout = 5 * time.Minute

type HTTPProvider struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (p HTTPProvider) ListTools() ([]Tool, error) {
	client := p.client()
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/tools", nil)
	if err != nil {
		return nil, err
	}
	p.authorize(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list mcp tools failed: %s", strings.TrimSpace(string(body)))
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

func (p HTTPProvider) CallTool(name string, input json.RawMessage, meta tools.CommandExecutionContext) (*CallResult, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	payload, _ := json.Marshal(ProviderToolCallRequest{
		ToolName: name,
		Input:    input,
		Meta:     meta,
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/call", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	p.authorize(req)
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("call mcp tool failed: %s", strings.TrimSpace(string(body)))
	}
	var out CallResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (p HTTPProvider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: defaultHTTPProviderTimeout}
}

func (p HTTPProvider) authorize(req *http.Request) {
	if token := strings.TrimSpace(p.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func strJSON(value string) []byte {
	payload, _ := json.Marshal(value)
	return payload
}
