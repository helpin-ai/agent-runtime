package tools

import (
	"context"
	"encoding/json"
	"testing"
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
