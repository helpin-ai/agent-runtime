package host

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestAdapterRegistryResolvesTargetsAndRegistersTools(t *testing.T) {
	ctx := context.Background()
	toolRegistry := tools.NewRegistry()
	adapters := NewAdapterRegistry(NewStaticContextProvider())

	adapter := fakeAppAdapter{appID: "host_app"}
	if err := adapters.Register(ctx, adapter, toolRegistry); err != nil {
		t.Fatalf("register adapter: %v", err)
	}

	targetContext, err := adapters.ResolveTarget(ctx, "host_app", agentcore.TargetRef{Type: "article", ID: "A-1"})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	if targetContext.Summary != "article context from host_app" {
		t.Fatalf("unexpected target summary: %q", targetContext.Summary)
	}

	out, err := toolRegistry.Execute(ctx, tools.CallContext{AppID: "host_app"}, "host_app_search", json.RawMessage(`{"query":"seo"}`))
	if err != nil {
		t.Fatalf("execute adapter tool: %v", err)
	}
	if string(out) != `{"ok":true}` {
		t.Fatalf("unexpected tool output: %s", string(out))
	}
}

func TestAdapterRegistryFallsBackForUnknownApp(t *testing.T) {
	ctx := context.Background()
	fallback := NewStaticContextProvider()
	target := agentcore.TargetRef{Type: "ticket", ID: "T-1"}
	fallback.Register("app-a", target, TargetContext{Summary: "fallback context"})
	adapters := NewAdapterRegistry(fallback)

	targetContext, err := adapters.ResolveTarget(ctx, "app-a", target)
	if err != nil {
		t.Fatalf("resolve fallback target: %v", err)
	}
	if targetContext.Summary != "fallback context" {
		t.Fatalf("unexpected fallback summary: %q", targetContext.Summary)
	}
}

type fakeAppAdapter struct {
	appID string
}

func (a fakeAppAdapter) AppID() string {
	return a.appID
}

func (a fakeAppAdapter) ResolveTarget(ctx context.Context, appID string, target agentcore.TargetRef) (*TargetContext, error) {
	return &TargetContext{
		Target:  target,
		Summary: target.Type + " context from " + appID,
	}, nil
}

func (a fakeAppAdapter) RegisterTools(ctx context.Context, registry *tools.Registry) error {
	registry.Register(tools.Definition{
		Name:        "host_app_search",
		Description: "Search host app context.",
		Category:    "Context",
		InputSchema: map[string]interface{}{"type": "object"},
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	})
	return nil
}
