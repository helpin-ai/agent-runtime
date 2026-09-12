package runtime

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Exercises the non-coding workflows through the real API-key model path with
// fixture host callbacks. No customer document or CRM record is modified.
func TestNativeNonCodingSmoke(t *testing.T) {
	if os.Getenv("AGENT_RUNTIME_NATIVE_SMOKE") != "1" {
		t.Skip("set AGENT_RUNTIME_NATIVE_SMOKE=1 with an OpenAI API key")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OPENAI_API_KEY is required")
	}
	for _, tt := range []struct{ name, tool, prompt string }{
		{"task_planner", "publish_task_plan_doc", "Publish a task plan for fixing a typo, including a verification step. Use publish_task_plan_doc, then summarize the result."},
		{"crm_operator", "add_deal_note", "Record a deal note stating that the customer requested an onboarding call next week. Use add_deal_note, then summarize the recorded update."},
		{"marketer", "create_document", "Create a short launch announcement for a support inbox feature. Use create_document, then summarize the document created."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			x := contextTestExec(t)
			x.Context = ctx
			x.Agent.ExecutionConfig = nil
			x.Agent.Provider = "openai"
			x.Agent.Model = firstNonEmpty(os.Getenv("NATIVE_SMOKE_MODEL"), defaultNativeOpenAIModel)
			x.Agent.ApprovalMode = agentcore.ApprovalModeNever
			called := false
			x.Tools.Register(tools.Definition{Name: tt.tool, Description: "Save the requested content in the fixture host record.", Mutating: true, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"content": map[string]any{"type": "string"}}, "required": []string{"content"}, "additionalProperties": false}}, func(_ context.Context, _ tools.CallContext, input json.RawMessage) (json.RawMessage, error) {
				var data struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal(input, &data); err != nil {
					return nil, err
				}
				called = data.Content != ""
				return json.RawMessage(`{"id":"fixture-record","saved":true}`), nil
			})
			x.AllowedTools = map[string]bool{tt.tool: true}
			x.Agent.AllowedTools = []string{tt.tool}
			x.Run.Input.Instructions = tt.prompt
			result, err := executeNativeModel(ctx, x, NativeConfig{ModelFactory: EinoProviderFactory{OpenAIAPIKey: key}, MaxToolSteps: 5})
			if err != nil {
				t.Fatal(err)
			}
			if !called || result.MaxSteps || result.AwaitingInput || result.AwaitingApproval {
				t.Fatal("workflow did not save its fixture host result and finish")
			}
		})
	}
}
