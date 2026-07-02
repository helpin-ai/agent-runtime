package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	appID      string
	token      string
	httpClient *http.Client
}

type ClientOption func(*Client)

func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

func WithServiceToken(token string) ClientOption {
	return func(c *Client) {
		c.token = strings.TrimSpace(token)
	}
}

func WithAppID(appID string) ClientOption {
	return func(c *Client) {
		c.appID = strings.TrimSpace(appID)
	}
}

func NewClient(baseURL string, opts ...ClientOption) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("agent runtime base URL is required")
	}
	client := &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(client)
		}
	}
	return client, nil
}

func (c *Client) AppID() string {
	if c == nil {
		return ""
	}
	return c.appID
}

func (c *Client) StartRun(ctx context.Context, req StartRunRequest) (*AgentRun, error) {
	if strings.TrimSpace(req.AppID) == "" {
		req.AppID = c.AppID()
	}
	var run AgentRun
	if err := c.doJSON(ctx, http.MethodPost, "/v1/runs", nil, req, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *Client) GetRun(ctx context.Context, runID string) (*AgentRun, error) {
	var run AgentRun
	if err := c.doJSON(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(strings.TrimSpace(runID)), c.appQuery(), nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *Client) ListRuns(ctx context.Context) ([]AgentRun, error) {
	var runs []AgentRun
	if err := c.doJSON(ctx, http.MethodGet, "/v1/runs", c.appQuery(), nil, &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

func (c *Client) ResumeRun(ctx context.Context, runID string, req ResumeRunRequest) (*AgentRun, error) {
	var run AgentRun
	if err := c.doJSON(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(strings.TrimSpace(runID))+"/resume", c.appQuery(), req, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *Client) ApproveRun(ctx context.Context, runID string, externalActorID ...string) (*AgentRun, error) {
	return c.ResumeRun(ctx, runID, ResumeRunRequest{
		Intent:          ResumeIntentApprove,
		ExternalActorID: firstOptionalString(externalActorID),
	})
}

func (c *Client) RequestChanges(ctx context.Context, runID, content string, externalActorID ...string) (*AgentRun, error) {
	return c.ResumeRun(ctx, runID, ResumeRunRequest{
		Intent:          ResumeIntentRequestChanges,
		Content:         content,
		ExternalActorID: firstOptionalString(externalActorID),
	})
}

func (c *Client) CancelRun(ctx context.Context, runID string) (*AgentRun, error) {
	var run AgentRun
	if err := c.doJSON(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(strings.TrimSpace(runID))+"/cancel", c.appQuery(), nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *Client) AppendMessage(ctx context.Context, runID string, req AppendMessageRequest) (*AgentRunMessage, error) {
	var message AgentRunMessage
	if err := c.doJSON(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(strings.TrimSpace(runID))+"/messages", c.appQuery(), req, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

func (c *Client) ListMessages(ctx context.Context, runID string) ([]AgentRunMessage, error) {
	var messages []AgentRunMessage
	if err := c.doJSON(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(strings.TrimSpace(runID))+"/messages", c.appQuery(), nil, &messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func (c *Client) AppendArtifact(ctx context.Context, runID string, req AppendArtifactRequest) (*AgentRunArtifact, error) {
	var artifact AgentRunArtifact
	if err := c.doJSON(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(strings.TrimSpace(runID))+"/artifacts", c.appQuery(), req, &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

func (c *Client) ListArtifacts(ctx context.Context, runID string) ([]AgentRunArtifact, error) {
	var artifacts []AgentRunArtifact
	if err := c.doJSON(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(strings.TrimSpace(runID))+"/artifacts", c.appQuery(), nil, &artifacts); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func (c *Client) ListInteractions(ctx context.Context, runID string) ([]AgentRunInteraction, error) {
	var interactions []AgentRunInteraction
	if err := c.doJSON(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(strings.TrimSpace(runID))+"/interactions", c.appQuery(), nil, &interactions); err != nil {
		return nil, err
	}
	return interactions, nil
}

func (c *Client) ListToolCalls(ctx context.Context, runID string) ([]ToolCall, error) {
	var calls []ToolCall
	if err := c.doJSON(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(strings.TrimSpace(runID))+"/tool-calls", c.appQuery(), nil, &calls); err != nil {
		return nil, err
	}
	return calls, nil
}

func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	var agents []Agent
	if err := c.doJSON(ctx, http.MethodGet, "/v1/agents", c.appQuery(), nil, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

func (c *Client) GetAgent(ctx context.Context, agentID string) (*Agent, error) {
	var agent Agent
	if err := c.doJSON(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(strings.TrimSpace(agentID)), c.appQuery(), nil, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

func (c *Client) UpsertAgent(ctx context.Context, agent Agent) (*Agent, error) {
	var out Agent
	if err := c.doJSON(ctx, http.MethodPut, "/v1/agents/"+url.PathEscape(strings.TrimSpace(agent.ID)), c.appQuery(), agent, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body interface{}, out interface{}) error {
	if c == nil {
		return fmt.Errorf("agent runtime client is not configured")
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode agent runtime request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call agent runtime: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("agent runtime %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode agent runtime response: %w", err)
	}
	return nil
}

func (c *Client) appQuery() url.Values {
	values := url.Values{}
	if appID := c.AppID(); appID != "" {
		values.Set("app_id", appID)
	}
	return values
}

func firstOptionalString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}
