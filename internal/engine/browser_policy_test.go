package engine

import (
	"encoding/json"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestStartRunFreezesTrustedBrowserPolicyBeforeDispatch(t *testing.T) {
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.ExecutionConfig = json.RawMessage(`{"browser":{"backend":"extension","profile":"existing-profile"}}`)
	if err := mem.CreateAgent(t.Context(), &agent); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	tools.RegisterBrowserTools(registry.ForApp("app-a"), tools.BrowserToolsConfig{Enabled: true, AppID: "app-a", ProfileRoot: t.TempDir()})
	targets := host.NewStaticContextProvider()
	targets.Register("app-a", agentcore.TargetRef{Type: "ticket", ID: "T-1"}, host.TargetContext{Summary: "test"})
	eng := New(Config{DefaultExecutionMode: ExecutionModeDurable, Store: mem, Durable: &recordingDurableExecutor{}, Targets: targets, Tools: registry})
	req := StartRunRequest{AppID: "app-a", AgentID: agent.ID, HostRunID: "same-request", Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"}, Metadata: map[string]interface{}{tools.BrowserPolicyMetadataKey: map[string]string{"backend": "standard"}}}
	run, err := eng.StartRun(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	agent.ExecutionConfig = json.RawMessage(`{"browser":{"backend":"standard"}}`)
	if err := mem.UpdateAgent(t.Context(), &agent); err != nil {
		t.Fatal(err)
	}
	persisted, err := mem.GetRun(t.Context(), "app-a", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(persisted.Input.Metadata[tools.BrowserPolicyMetadataKey])
	var policy tools.BrowserPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Backend != "extension" || policy.Profile != "existing-profile" {
		t.Fatalf("policy=%+v", policy)
	}
	duplicate, err := eng.StartRun(t.Context(), req)
	if err != nil || duplicate.ID != run.ID {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	raw, _ = json.Marshal(duplicate.Input.Metadata[tools.BrowserPolicyMetadataKey])
	_ = json.Unmarshal(raw, &policy)
	if policy.Backend != "extension" {
		t.Fatal("duplicate start changed policy")
	}
}
