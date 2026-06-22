package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type HTTPWorkspaceLookup struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (l HTTPWorkspaceLookup) GetByID(ctx context.Context, appID, id string) (*WorkspaceSkill, error) {
	return l.GetByIDForContext(ctx, LookupRequest{LookupContext: LookupContext{AppID: appID}, SkillID: id})
}

func (l HTTPWorkspaceLookup) GetActiveByKey(ctx context.Context, appID, key string) (*WorkspaceSkill, error) {
	return l.GetActiveByKeyForContext(ctx, LookupRequest{LookupContext: LookupContext{AppID: appID}, Key: key})
}

func (l HTTPWorkspaceLookup) GetByIDForContext(ctx context.Context, req LookupRequest) (*WorkspaceSkill, error) {
	req.SkillID = strings.TrimSpace(req.SkillID)
	if req.SkillID == "" {
		return nil, fmt.Errorf("skill_id is required")
	}
	return l.post(ctx, "/by-id", req)
}

func (l HTTPWorkspaceLookup) GetActiveByKeyForContext(ctx context.Context, req LookupRequest) (*WorkspaceSkill, error) {
	req.Key = strings.TrimSpace(req.Key)
	if req.Key == "" {
		return nil, fmt.Errorf("skill key is required")
	}
	return l.post(ctx, "/active-by-key", req)
}

func (l HTTPWorkspaceLookup) post(ctx context.Context, path string, req LookupRequest) (*WorkspaceSkill, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(l.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("workspace skill lookup base_url is required")
	}
	req.AppID = strings.TrimSpace(req.AppID)
	if req.AppID == "" {
		return nil, fmt.Errorf("app_id is required")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(l.Token); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := l.client().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("workspace skill lookup %s failed: %s", path, strings.TrimSpace(string(body)))
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	skill, err := decodeWorkspaceSkillResponse(body)
	if err != nil {
		return nil, fmt.Errorf("decode workspace skill lookup response: %w", err)
	}
	return skill, nil
}

func (l HTTPWorkspaceLookup) client() *http.Client {
	if l.Client != nil {
		return l.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func decodeWorkspaceSkillResponse(body []byte) (*WorkspaceSkill, error) {
	var direct WorkspaceSkill
	if err := json.Unmarshal(body, &direct); err == nil && (strings.TrimSpace(direct.ID) != "" || strings.TrimSpace(direct.Key) != "") {
		return &direct, nil
	}
	var wrapped struct {
		Skill *WorkspaceSkill `json:"skill"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Skill, nil
}

var _ WorkspaceLookup = HTTPWorkspaceLookup{}
var _ ContextualWorkspaceLookup = HTTPWorkspaceLookup{}
