package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestRegistryScopesHostToolsByApp(t *testing.T) {
	registry := NewRegistry()
	register := func(appID, value string) {
		registry.ForApp(appID).Register(Definition{Name: "lookup", Category: "Host"}, func(context.Context, CallContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"app":"` + value + `"}`), nil
		})
	}
	register("app-a", "a")
	register("app-b", "b")

	for appID, expected := range map[string]string{"app-a": `{"app":"a"}`, "app-b": `{"app":"b"}`} {
		if _, ok := registry.DefinitionForApp(appID, "lookup"); !ok {
			t.Fatalf("expected lookup definition for %s", appID)
		}
		out, err := registry.Execute(context.Background(), CallContext{AppID: appID}, "lookup", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("execute %s lookup: %v", appID, err)
		}
		if string(out) != expected {
			t.Fatalf("%s routed to wrong handler: %s", appID, out)
		}
	}
	if _, ok := registry.DefinitionForApp("app-c", "lookup"); ok {
		t.Fatal("app-scoped tool leaked into an unconfigured app")
	}
	if _, err := registry.Execute(context.Background(), CallContext{AppID: "app-c"}, "lookup", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected unconfigured app tool execution to fail")
	}
}

func TestProviderRefresherUsesSingleFlightAndCooldown(t *testing.T) {
	registry := NewRegistry()
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	registry.RegisterProviderRefresher("helpin", "helpin", time.Minute, func(context.Context) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return nil
	})
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		if err := registry.RefreshProvider(context.Background(), "helpin", "helpin"); err != nil {
			t.Errorf("scheduled refresh: %v", err)
		}
	}()
	<-started
	for i := 0; i < 5; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			registry.RefreshProvidersForApp(context.Background(), "helpin")
		}()
	}
	close(release)
	group.Wait()
	registry.RefreshProvidersForApp(context.Background(), "helpin")
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls=%d, want one single-flight call inside cooldown", got)
	}
}

func TestReplaceAppProviderRejectsInvalidCatalog(t *testing.T) {
	registry := NewRegistry()
	tests := map[string][]ProviderRegistration{
		"nil handler": {{Definition: Definition{Name: "create_collection"}}},
	}
	for name, registrations := range tests {
		t.Run(name, func(t *testing.T) {
			if err := registry.ReplaceAppProvider("helpin", "helpin", 0, registrations, false); err == nil {
				t.Fatal("expected invalid provider catalog to be rejected")
			}
		})
	}
}

func TestRegistryProviderPrecedence(t *testing.T) {
	registry := NewRegistry()
	registry.ForApp("helpin").Register(Definition{Name: "create_collection"}, func(context.Context, CallContext, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"source":"static"}`), nil
	})
	err := registry.ReplaceAppProvider("helpin", "helpin", 0, []ProviderRegistration{{
		Definition: Definition{Name: "create_collection"},
		Handler: func(context.Context, CallContext, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"source":"mcp"}`), nil
		},
	}}, false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := registry.Execute(context.Background(), CallContext{AppID: "helpin"}, "create_collection", nil)
	if err != nil || string(out) != `{"source":"mcp"}` {
		t.Fatalf("provider did not override app-scoped tool: output=%s err=%v", out, err)
	}
	agent := &agentcore.Agent{AllowedTools: []string{"create_collection"}}
	allowed := AllowedSet(agent, nil)
	if len(allowed) != 1 || !allowed["create_collection"] {
		t.Fatalf("canonical tool was not allowed: %#v", allowed)
	}
}

func TestRegistryConcurrentProviderRefreshAndReads(t *testing.T) {
	registry := NewRegistry()
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for iteration := 0; iteration < 100; iteration++ {
				name := fmt.Sprintf("tool_%d", iteration%2)
				if worker%2 == 0 {
					_ = registry.ReplaceAppProvider("helpin", "helpin", 0, []ProviderRegistration{{Definition: Definition{Name: name}, Handler: func(context.Context, CallContext, json.RawMessage) (json.RawMessage, error) { return nil, nil }}}, false)
					continue
				}
				_ = registry.DefinitionsForApp("helpin")
				_, _ = registry.DefinitionForApp("helpin", name)
				_ = registry.CloneForApp("helpin")
			}
		}(worker)
	}
	group.Wait()
}
