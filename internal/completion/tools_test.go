package completion

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestRequiredToolsMergesAgentAndRunContracts(t *testing.T) {
	agent := &agentcore.Agent{ExecutionConfig: json.RawMessage(`{"completion":{"required_tools":["publish_task_plan_doc"]}}`)}
	run := &agentcore.AgentRun{Input: agentcore.RunInput{Metadata: map[string]interface{}{
		"completion_required_tools": []interface{}{"mcp__helpin__complete_support_coverage_gap", "publish_task_plan_doc"},
	}}}

	got := RequiredTools(agent, run)
	if strings.Join(got, ",") != "complete_support_coverage_gap,publish_task_plan_doc" {
		t.Fatalf("RequiredTools() = %#v", got)
	}
}
