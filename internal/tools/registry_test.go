package tools

import "testing"

func TestCanonicalNameRemovesMCPTransportNamespace(t *testing.T) {
	tests := map[string]string{
		"publish_task_plan_doc":                     "publish_task_plan_doc",
		"helpin/publish_task_plan_doc":              "publish_task_plan_doc",
		"agent_runtime/request_approval":            "request_approval",
		"mcp__helpin__publish_task_plan_doc":        "publish_task_plan_doc",
		"mcp__agent_runtime__request_approval":      "request_approval",
		"mcp__future_app__custom__approval_request": "custom__approval_request",
		"mcp__future_app__request_human_input":      "request_user_input",
	}
	for raw, want := range tests {
		if got := CanonicalName(raw); got != want {
			t.Fatalf("CanonicalName(%q) = %q, want %q", raw, got, want)
		}
	}
}
