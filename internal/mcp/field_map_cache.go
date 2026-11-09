package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"sync"
	"time"
)

// FieldMapCache holds only compact schemas, not CRM records. New connections
// authenticate before using it; credential rotation changes the partition.
type FieldMapCache struct {
	mu         sync.Mutex
	entries    map[string]fieldMapEntry
	pending    map[string]chan struct{}
	generation uint64
	now        func() time.Time
}
type fieldMapEntry struct {
	scope, workspace, revision string
	value                      json.RawMessage
	expires                    time.Time
}

func NewFieldMapCache() *FieldMapCache {
	return &FieldMapCache{entries: map[string]fieldMapEntry{}, pending: map[string]chan struct{}{}, now: time.Now}
}
func fieldMapScope(appID string, server agentcore.RunMCPServer, credential *RunCredential) string {
	raw, _ := json.Marshal([]any{appID, server.ServerID, server.URL, credential})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
func (c *FieldMapCache) invalidate(scope string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	for key, entry := range c.entries {
		if entry.scope == scope {
			delete(c.entries, key)
		}
	}
}
func (c *FieldMapCache) load(ctx context.Context, scope string, args map[string]any, fetch func() (json.RawMessage, error)) (json.RawMessage, error) {
	if c == nil {
		return fetch()
	}
	encoded, _ := json.Marshal(args)
	key := scope + ":" + fmt.Sprintf("%x", sha256.Sum256(encoded))
	for {
		c.mu.Lock()
		if entry, ok := c.entries[key]; ok && c.now().Before(entry.expires) {
			out := append(json.RawMessage(nil), entry.value...)
			c.mu.Unlock()
			return out, nil
		}
		if done := c.pending[key]; done != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		c.pending[key] = done
		generation := c.generation
		c.mu.Unlock()
		value, err := fetch()
		workspace, revision := fieldMapIdentity(value)
		c.mu.Lock()
		if err == nil && workspace != "" && revision != "" && generation == c.generation {
			if len(c.entries) >= 128 {
				var oldest string
				var expiry time.Time
				for candidate, entry := range c.entries {
					if oldest == "" || entry.expires.Before(expiry) {
						oldest, expiry = candidate, entry.expires
					}
				}
				delete(c.entries, oldest)
			}
			c.entries[key] = fieldMapEntry{scope: scope, workspace: workspace, revision: revision, value: append(json.RawMessage(nil), value...), expires: c.now().Add(time.Minute)}
		}
		delete(c.pending, key)
		close(done)
		c.mu.Unlock()
		return value, err
	}
}
func fieldMapIdentity(raw json.RawMessage) (string, string) {
	var envelope struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return "", ""
	}
	var body map[string]any
	if json.Unmarshal([]byte(envelope.Text), &body) != nil {
		return "", ""
	}
	if data, ok := body["data"].(map[string]any); ok {
		body = data
	}
	workspace, _ := body["workspace_id"].(string)
	revision, _ := body["schema_revision"].(string)
	return workspace, revision
}
