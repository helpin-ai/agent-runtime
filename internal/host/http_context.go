package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type HTTPContextProvider struct {
	Endpoint string
	Token    string
	Client   *http.Client
}

func (p HTTPContextProvider) ResolveTarget(ctx context.Context, appID string, target agentcore.TargetRef) (*TargetContext, error) {
	return p.ResolveRunTarget(ctx, TargetContextRequest{AppID: appID, Target: target})
}

func (p HTTPContextProvider) ResolveRunTarget(ctx context.Context, req TargetContextRequest) (*TargetContext, error) {
	endpoint := strings.TrimSpace(p.Endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("target context endpoint is required")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(p.Token); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.client().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("target context adapter failed: %s", strings.TrimSpace(string(body)))
	}
	var out TargetContext
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode target context response: %w", err)
	}
	if strings.TrimSpace(out.Target.Type) == "" || strings.TrimSpace(out.Target.ID) == "" {
		out.Target = req.Target
	}
	if out.Data == nil {
		out.Data = map[string]interface{}{}
	}
	return &out, nil
}

func (p HTTPContextProvider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

var _ TargetContextProvider = HTTPContextProvider{}
var _ RunTargetContextProvider = HTTPContextProvider{}
