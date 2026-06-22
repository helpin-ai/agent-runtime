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

	adapter := fakeAppAdapter{appID: "contentpen"}
	if err := adapters.Register(ctx, adapter, toolRegistry); err != nil {
		t.Fatalf("register adapter: %v", err)
	}

	targetContext, err := adapters.ResolveTarget(ctx, "contentpen", agentcore.TargetRef{Type: "article", ID: "A-1"})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	if targetContext.Summary != "article context from contentpen" {
		t.Fatalf("unexpected target summary: %q", targetContext.Summary)
	}

	out, err := toolRegistry.Execute(ctx, tools.CallContext{}, "contentpen_search", json.RawMessage(`{"query":"seo"}`))
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

func TestConfiguredAppAdapterRegistersCommandTools(t *testing.T) {
	ctx := context.Background()
	toolRegistry := tools.NewRegistry()
	adapters := NewAdapterRegistry(NewStaticContextProvider())
	var gotCommand string
	adapter := ConfiguredAppAdapter{
		ID: "helpin",
		CommandExecutor: tools.CommandToolExecutorFunc(func(ctx context.Context, meta tools.CommandExecutionContext, commandName string, input json.RawMessage) (json.RawMessage, error) {
			gotCommand = commandName
			if meta.AppID != "helpin" || meta.RunID != "run-1" || meta.TargetType != "task" || meta.TargetID != "task-1" {
				t.Fatalf("unexpected command meta: %#v", meta)
			}
			return json.RawMessage(`{"ok":true}`), nil
		}),
		CommandTools: []tools.CommandToolMetadata{{
			CommandName: "pm.update_task_state",
			Alias:       "update_task_state",
			Category:    "PM / Tasks",
			Description: "Update task state.",
			InputSchema: map[string]any{"type": "object"},
			Mutating:    true,
		}},
	}
	if err := adapters.Register(ctx, adapter, toolRegistry); err != nil {
		t.Fatalf("register adapter: %v", err)
	}
	def, ok := toolRegistry.Definition("update_task_state")
	if !ok || !def.Mutating {
		t.Fatalf("expected mutating update_task_state definition, got %#v", def)
	}
	run := &agentcore.AgentRun{
		ID:     "run-1",
		AppID:  "helpin",
		Target: agentcore.TargetRef{Type: "task", ID: "task-1"},
	}
	out, err := toolRegistry.Execute(ctx, tools.CallContext{AppID: "helpin", RunID: "run-1", Run: run}, "update_task_state", json.RawMessage(`{"state_id":"done"}`))
	if err != nil {
		t.Fatalf("execute command tool: %v", err)
	}
	if gotCommand != "pm.update_task_state" || string(out) != `{"ok":true}` {
		t.Fatalf("unexpected command execution: command=%q out=%s", gotCommand, string(out))
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
		Name:        "contentpen_search",
		Description: "Search ContentPen context.",
		Category:    "Context",
		InputSchema: map[string]interface{}{"type": "object"},
	}, func(ctx context.Context, callCtx tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	})
	return nil
}
