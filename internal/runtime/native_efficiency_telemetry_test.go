package runtime

import (
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"testing"
)

func TestWorkerUsageHasDurableScopeAndPurpose(t *testing.T) {
	x := contextTestExec(t)
	sink := &testEventSink{}
	x.EventSink = sink
	x.Run.Input.Metadata = map[string]any{"work_attempt_id": "attempt"}
	x.Context = agentcore.WithExecutionGeneration(x.Context, 1)
	x.Store = ownershipStore{Store: x.Store, NativeStateStore: x.Store.(agentcore.NativeStateStore), attempt: &agentcore.WorkAttempt{ID: "attempt", JobID: "job", State: "open", Generation: 1, ItemIDs: []string{"one"}}, job: &agentcore.WorkJob{State: "running"}, items: []agentcore.WorkItem{{ID: "one", AttemptID: "attempt", State: "running"}}}
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	messages, result, err := r.initialMessages(true)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		result = &nativeExecutionResult{}
	}
	result.Messages = messages
	if err := r.checkpointUsage(x.Context, result, &NativeModelResponse{Usage: NativeUsage{InputTokens: 100, OutputTokens: 10}}, "agent"); err != nil {
		t.Fatal(err)
	}
	loaded, err := openNativeRecorder(x.Context, x, true)
	if err != nil || loaded.state.UsageByScope["item:one"].InputTokens != 100 {
		t.Fatal("scope usage not durable")
	}
	for _, e := range sink.events {
		if e.Type == "usage.checkpoint" && e.Data["purpose"] == "agent" && e.Data["scope"] == "item:one" {
			return
		}
	}
	t.Fatal("usage event lacks item attribution and request purpose")
}
