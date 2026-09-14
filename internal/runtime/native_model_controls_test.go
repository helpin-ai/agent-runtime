package runtime

import (
	"encoding/json"
	"testing"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestRunControlsReplaceLegacyEvenWhenEmpty(t *testing.T) {
	legacy := json.RawMessage(`{"reasoning_effort":"high","service_tier":"fast","max_tool_steps":42,"native_context":{"enabled":true},"openrouter":{"provider":{"quantizations":["fp8"]}}}`)
	exec := &ExecutionContext{Agent: &agentcore.Agent{ExecutionConfig: legacy}, Run: &agentcore.AgentRun{}}
	effort, tier, err := nativeModelControls(exec, "openai")
	if err == nil {
		t.Fatal("legacy OpenRouter controls on openai must be rejected")
	}
	exec.Run.Input.Model = &sdk.RunModel{Provider: "openai", Model: "custom", Controls: &sdk.ModelControls{}}
	effort, tier, err = nativeModelControls(exec, "openai")
	if err != nil || effort != nil || tier != nil || openRouterExtraFields(exec) != nil {
		t.Fatalf("empty override inherited controls: %v %v %v", effort, tier, err)
	}
	if string(exec.Agent.ExecutionConfig) != string(legacy) {
		t.Fatal("model override modified agent execution settings")
	}
	low := "low"
	exec.Run.Input.Model.Controls = &sdk.ModelControls{ReasoningEffort: &low}
	effort, tier, err = nativeModelControls(exec, "openai")
	if err != nil || effort == nil || string(effort.Effort) != "low" || tier != nil {
		t.Fatalf("explicit controls=%v %v %v", effort, tier, err)
	}
}

func TestChatGPTDoesNotAdvertiseLosslessReplay(t *testing.T) {
	found := false
	for _, provider := range NativeProviderCapabilities() {
		if provider.Name == "openai_chatgpt" {
			found = true
			if provider.LosslessResponseReplay {
				t.Fatal("ChatGPT inherited OpenAI response replay")
			}
		}
	}
	if !found {
		t.Fatal("ChatGPT capability missing")
	}
}
