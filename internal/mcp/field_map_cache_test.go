package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFieldMapReuseAcrossWorkers(t *testing.T) {
	var fetches atomic.Int32
	remote := protocol.NewServer(&protocol.Implementation{Name: "crm", Version: "1"}, nil)
	remote.AddTool(&protocol.Tool{Name: "describe_crm_fields", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"object_type": map[string]any{"type": "string"}, "compact": map[string]any{"type": "boolean"}, "refresh": map[string]any{"type": "boolean"}}}}, func(context.Context, *protocol.CallToolRequest) (*protocol.CallToolResult, error) {
		fetches.Add(1)
		return &protocol.CallToolResult{Content: []protocol.Content{&protocol.TextContent{Text: `{"data":{"workspace_id":"workspace-one","schema_revision":"version-one","fields":[]}}`}}}, nil
	})
	server := httptest.NewServer(protocol.NewStreamableHTTPHandler(func(*http.Request) *protocol.Server { return remote }, &protocol.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer server.Close()
	cfg := RunConfig{FieldMaps: NewFieldMapCache(), CredentialKey: []byte("0123456789abcdef0123456789abcdef"), AllowHTTP: true, AllowPrivateNetwork: true, ConnectTimeout: time.Second}
	memory := store.NewMemory()
	ctx := context.Background()
	for _, runID := range []string{"worker-one", "worker-two", "worker-three"} {
		stored, err := PrepareStoredServers("app", runID, []RunServerRequest{{ServerID: "crm", ServerName: "crm", Transport: agentcore.MCPTransportStreamableHTTP, URL: server.URL, Tools: []RunTool{{Name: "describe_crm_fields", Access: "read"}}}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		run := &agentcore.AgentRun{ID: runID, AppID: "app", Input: agentcore.RunInput{Metadata: map[string]any{"work_attempt_id": "attempt"}}}
		if err := memory.CreateRunWithMCP(ctx, run, stored); err != nil {
			t.Fatal(err)
		}
		registry, _, _, closeTools, err := PrepareRunTools(ctx, memory, tools.NewRegistry(), "app", runID, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if _, err := registry.Execute(ctx, tools.CallContext{AppID: "app", RunID: runID, Run: run}, "mcp__crm__describe_crm_fields", json.RawMessage(`{"object_type":"company","compact":true}`)); err != nil {
				t.Fatal(err)
			}
		}
		closeTools()
	}
	t.Logf("workers=3 requests=6 provider_catalog_fetches=%d", fetches.Load())
	if fetches.Load() != 1 {
		t.Fatalf("catalog was not shared across workers: %d provider fetches", fetches.Load())
	}
}

func TestFieldMapCacheIsolationExpiryAndInvalidation(t *testing.T) {
	cache := NewFieldMapCache()
	now := time.Now()
	cache.now = func() time.Time { return now }
	calls := 0
	fetch := func() (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"text":"{\"workspace_id\":\"w\",\"schema_revision\":\"v1\"}"}`), nil
	}
	ctx := context.Background()
	load := func(scope, kind string) {
		t.Helper()
		if _, err := cache.load(ctx, scope, map[string]any{"object_type": kind}, fetch); err != nil {
			t.Fatal(err)
		}
	}
	load("a", "company")
	load("a", "company")
	load("b", "company")
	load("a", "contact")
	if calls != 3 {
		t.Fatalf("partition fetches=%d", calls)
	}
	cache.invalidate("a")
	load("b", "company")
	load("a", "company")
	if calls != 4 {
		t.Fatalf("invalidation fetches=%d", calls)
	}
	now = now.Add(time.Minute)
	load("a", "company")
	if calls != 5 {
		t.Fatalf("expiry fetches=%d", calls)
	}
	for i := 0; i < 140; i++ {
		load(fmt.Sprint(i), "company")
	}
	if len(cache.entries) > 128 {
		t.Fatalf("unbounded entries: %d", len(cache.entries))
	}
}

func TestFieldMapCacheRejectsStaleInflightAndCancelsWaiter(t *testing.T) {
	cache := NewFieldMapCache()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cache.load(context.Background(), "a", nil, func() (json.RawMessage, error) {
			close(entered)
			<-release
			return json.RawMessage(`{"text":"{\"workspace_id\":\"w\",\"schema_revision\":\"v1\"}"}`), nil
		})
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.load(ctx, "a", nil, func() (json.RawMessage, error) { t.Error("waiter fetched"); return nil, nil }); err != context.Canceled {
		t.Fatalf("waiter error=%v", err)
	}
	cache.invalidate("a")
	close(release)
	<-done
	if len(cache.entries) != 0 {
		t.Fatal("invalidated in-flight result was cached")
	}
}

func TestFieldMapScopeSeparatesCredentialsAppsAndEndpoints(t *testing.T) {
	base := agentcore.RunMCPServer{ServerID: "crm", URL: "https://one.test"}
	first := fieldMapScope("app", base, nil)
	changed := base
	changed.URL = "https://two.test"
	for _, scope := range []string{fieldMapScope("other", base, nil), fieldMapScope("app", changed, nil), fieldMapScope("app", base, &RunCredential{})} {
		if scope == first {
			t.Fatal("cache crossed authentication boundary")
		}
	}
}

func TestFieldMapCacheCoalescesConcurrentWorkers(t *testing.T) {
	cache := NewFieldMapCache()
	var fetches atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := cache.load(context.Background(), "shared", nil, func() (json.RawMessage, error) {
				fetches.Add(1)
				return json.RawMessage(`{"text":"{\"workspace_id\":\"w\",\"schema_revision\":\"v1\"}"}`), nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if fetches.Load() != 1 {
		t.Fatalf("fetches=%d", fetches.Load())
	}
}
