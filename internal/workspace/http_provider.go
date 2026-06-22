package workspace

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

type HTTPProvider struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (p HTTPProvider) PrepareWorkspace(ctx context.Context, req PrepareRequest) (*agentcore.WorkspaceLease, error) {
	var lease agentcore.WorkspaceLease
	if err := p.post(ctx, "prepare", req, &lease); err != nil {
		return nil, err
	}
	NormalizeLease(&lease)
	if lease.ID == "" {
		return nil, fmt.Errorf("workspace provider returned empty lease id")
	}
	if lease.RootPath == "" {
		return nil, fmt.Errorf("workspace provider returned empty root_path")
	}
	return &lease, nil
}

func (p HTTPProvider) FinalizeWorkspace(ctx context.Context, req FinalizeRequest) (*FinalizeResult, error) {
	var result FinalizeResult
	if err := p.post(ctx, "finalize", req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (p HTTPProvider) CleanupWorkspace(ctx context.Context, req CleanupRequest) error {
	return p.post(ctx, "cleanup", req, nil)
}

func (p HTTPProvider) ResolveRepositoryWorkspace(ctx context.Context, req PrepareRequest) (*RepositoryWorkspaceSpec, error) {
	var spec RepositoryWorkspaceSpec
	if err := p.post(ctx, "repository-spec", req, &spec); err != nil {
		return nil, err
	}
	NormalizeRepositorySpec(&spec)
	return &spec, nil
}

func (p HTTPProvider) post(ctx context.Context, path string, input interface{}, output interface{}) error {
	baseURL := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("workspace provider base_url is required")
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
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
		return fmt.Errorf("workspace provider %s failed: %s", path, strings.TrimSpace(string(body)))
	}
	if output != nil && len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, output); err != nil {
			return err
		}
	}
	return nil
}

func (p HTTPProvider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}
