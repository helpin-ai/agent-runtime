package workjobs

import (
	"encoding/json"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"strings"
	"testing"
)

func TestRetryWorkerReceivesSavedEvidence(t *testing.T) {
	job := &agentcore.WorkJob{Spec: agentcore.WorkJobSpec{Objective: "verify companies", ResultSchema: json.RawMessage(`{"type":"object"}`)}}
	text, err := workerInstructions(job, []agentcore.WorkItem{{ID: "one", Input: json.RawMessage(`{"name":"Example"}`), Result: json.RawMessage(`{"verified_url":"https://example.test/source","receipt":"write-42"}`), Exception: &agentcore.WorkItemException{Kind: "source_unavailable", Detail: "one remaining source"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"previous_result", "https://example.test/source", "write-42", "previous_exception", "execution blocker", "never research or mutate it again"} {
		if !strings.Contains(text, required) {
			t.Fatalf("retry contract lost %q", required)
		}
	}
}
