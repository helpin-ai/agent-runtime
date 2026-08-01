package tools

import "testing"

func TestCanonicalNameNormalizesRuntimeOwnedMCPTransportNames(t *testing.T) {
	tests := map[string]string{
		"publish_task_plan_doc":                "publish_task_plan_doc",
		"mcp__helpin__publish_task_plan_doc":   "publish_task_plan_doc",
		"mcp__agent_runtime__request_approval": "request_approval",
		"request_human_input":                  "request_user_input",
		"mcp__future_app__custom_tool":         "mcp__future_app__custom_tool",
	}
	for raw, want := range tests {
		if got := CanonicalName(raw); got != want {
			t.Fatalf("CanonicalName(%q) = %q, want %q", raw, got, want)
		}
	}
}
