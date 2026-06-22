package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type HTTPCommandExecutor struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

type CommandExecutionRequest struct {
	Meta        CommandExecutionContext `json:"meta"`
	CommandName string                  `json:"command_name"`
	Input       json.RawMessage         `json:"input,omitempty"`
}

type CommandExecutionResponse struct {
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func (e HTTPCommandExecutor) ExecuteCommand(ctx context.Context, meta CommandExecutionContext, commandName string, input json.RawMessage) (json.RawMessage, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("command executor base_url is required")
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	payload, err := json.Marshal(CommandExecutionRequest{
		Meta:        meta,
		CommandName: strings.TrimSpace(commandName),
		Input:       input,
	})
	if err != nil {
		return nil, fmt.Errorf("encode command request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/execute", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(e.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("command executor returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var decoded CommandExecutionResponse
	if err := json.Unmarshal(body, &decoded); err == nil && (len(decoded.Output) > 0 || strings.TrimSpace(decoded.Error) != "") {
		if strings.TrimSpace(decoded.Error) != "" {
			return nil, errors.New(decoded.Error)
		}
		return decoded.Output, nil
	}
	if !json.Valid(body) {
		return json.Marshal(strings.TrimSpace(string(body)))
	}
	return json.RawMessage(body), nil
}
