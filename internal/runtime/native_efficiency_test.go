package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// This replay uses deterministic responses, not a paid model. Counts are the
// runtime's request estimates and must not be reported as vendor billing.
type efficiencyModel struct {
	contextTestModel
	agentInput, summaryInput int
}

func (m *efficiencyModel) Generate(ctx context.Context, req NativeModelRequest) (*NativeModelResponse, error) {
	m.agentInput += nativeRequestTokens(req.SystemPrompt, req.Messages, req.Tools)
	return m.contextTestModel.Generate(ctx, req)
}
func (m *efficiencyModel) Summarize(ctx context.Context, prompt string, limit int) (*NativeModelResponse, error) {
	m.summaryInput += (len(prompt)+2)/3 + 32
	return m.contextTestModel.Summarize(ctx, prompt, limit)
}
func TestWorkerContextEfficiencyReplay(t *testing.T) {
	x := contextTestExec(t)
	x.Agent.SystemPrompt = strings.Repeat("Required task policy. ", 1000)
	m := &efficiencyModel{contextTestModel: contextTestModel{rounds: 22}}
	result, err := executeNativeModel(x.Context, x, NativeConfig{ModelFactory: fakeNativeFactory{model: m}, MaxToolSteps: 25})
	executionErr := err
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	report := map[string]any{"model_calls": len(m.requests), "summary_calls": m.summaries, "committed_compactions": r.state.Generation, "estimated_agent_input": m.agentInput, "estimated_summary_input": m.summaryInput, "estimated_total_input": m.agentInput + m.summaryInput, "final_answer": result.AssistantText, "execution_error": fmt.Sprint(executionErr)}
	raw, _ := json.Marshal(report)
	t.Log(string(raw))
	for _, req := range m.requests {
		assertCompleteToolGroups(t, req.Messages)
	}
	if executionErr != nil {
		t.Fatal(executionErr)
	}
	if result.AssistantText != "done" {
		t.Fatal("lost final answer")
	}
}
