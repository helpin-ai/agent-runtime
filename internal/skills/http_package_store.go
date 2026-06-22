package skills

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HTTPPackageStore struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func (s HTTPPackageStore) GetObject(ctx context.Context, key string) ([]byte, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("package object key is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("skill package store base_url is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/objects/"+url.PathEscape(key), nil)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(s.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("skill package store failed: %s", strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (s HTTPPackageStore) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

var _ PackageStore = HTTPPackageStore{}
