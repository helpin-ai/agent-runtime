package engine

import (
	"context"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestResumeNotificationPersistsOriginBeforeSignal(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	if err := mem.CreateAgent(ctx, &agentcore.Agent{ID: "agent", AppID: "a", RuntimeKind: agentcore.RuntimeNativeSDK}); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{ID: "r", AppID: "a", AgentID: "agent", RuntimeKind: agentcore.RuntimeNativeSDK, Status: agentcore.RunStatusPaused, PauseReason: agentcore.PauseReasonUserMessage, ExecutionMode: ExecutionModeDurable}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	durable := &recordingDurableExecutor{store: mem}
	eng := New(Config{Store: mem, DefaultExecutionMode: ExecutionModeDurable, Runtimes: runtime.NewRegistry(&recordingRuntimeAdapter{}), Tools: tools.NewRegistry(), Targets: host.NewStaticContextProvider(), Durable: durable})
	payload := ResumePayload{Intent: "reply", Content: "Child completed", ResumeID: "notification-1", MessageProvenance: "system_notification"}
	if _, err := eng.ResumeRun(ctx, "a", "r", payload); err != nil {
		t.Fatal(err)
	}
	last := durable.runAtResume.Input.Metadata["last_resume"].(map[string]interface{})
	if last["message_provenance"] != "system_notification" {
		t.Fatalf("origin lost: %+v", last)
	}
	messages, err := mem.ListMessages(ctx, "a", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].MessageType != "system_notification" {
		t.Fatalf("transcript origin lost: %+v", messages)
	}
	if _, err := eng.ResumeRun(ctx, "a", "r", payload); err != nil {
		t.Fatal(err)
	}
	if durable.resumeCalls != 1 {
		t.Fatal("notification replay resumed twice")
	}
}

func TestResumeNotificationCannotResolveInteraction(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{agentcore.PauseReasonHumanApproval, agentcore.PauseReasonHumanInput, agentcore.PauseReasonAuth} {
		mem := store.NewMemory()
		run := &agentcore.AgentRun{ID: "r", AppID: "a", Status: agentcore.RunStatusPaused, PauseReason: reason}
		if err := mem.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		eng := testEngine(mem, host.NewStaticContextProvider())
		if _, err := eng.ResumeRun(ctx, "a", "r", ResumePayload{Intent: "reply", MessageProvenance: "system_notification", Content: "approved"}); err == nil {
			t.Fatal("notification resolved interaction")
		}
		stored, err := mem.GetRun(ctx, "a", "r")
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != agentcore.RunStatusPaused {
			t.Fatal("rejected notification changed lifecycle")
		}
	}
}

func TestResumeNotificationChecksPendingRecordsEvenWhenPauseReasonIsStale(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	if err := mem.CreateRun(ctx, &agentcore.AgentRun{ID: "r", AppID: "a", Status: agentcore.RunStatusPaused, PauseReason: agentcore.PauseReasonUserMessage}); err != nil {
		t.Fatal(err)
	}
	if err := mem.AppendInteraction(ctx, &agentcore.AgentRunInteraction{ID: "approval", AppID: "a", RunID: "r", Status: "pending", InteractionKind: "approval"}); err != nil {
		t.Fatal(err)
	}
	eng := testEngine(mem, host.NewStaticContextProvider())
	if _, err := eng.ResumeRun(ctx, "a", "r", ResumePayload{Intent: "reply", MessageProvenance: "system_notification", Content: "Child completed"}); err == nil {
		t.Fatal("notification resolved a pending record")
	}
	interactions, err := mem.ListInteractions(ctx, "a", "r")
	if err != nil || len(interactions) != 1 || interactions[0].Status != "pending" {
		t.Fatalf("interaction changed: %+v, %v", interactions, err)
	}
	messages, err := mem.ListMessages(ctx, "a", "r")
	if err != nil || len(messages) != 0 {
		t.Fatalf("rejected notification was appended: %+v, %v", messages, err)
	}
}

func TestResumeProvenanceValidation(t *testing.T) {
	for _, payload := range []ResumePayload{
		{Intent: "reply", MessageProvenance: "untrusted"},
		{Intent: "reply", MessageProvenance: "human"},
		{Intent: "auth_completed", MessageProvenance: "human", ExternalActorID: "user-1"},
		{Intent: "approve", MessageProvenance: "system_notification"},
		{Intent: "reply", MessageProvenance: "system_notification", InteractionID: "approval"},
		{Intent: "reply", MessageProvenance: "system_notification", ResponsePayload: []byte(`{"approved":true}`)},
	} {
		ctx := context.Background()
		mem := store.NewMemory()
		if err := mem.CreateRun(ctx, &agentcore.AgentRun{ID: "r", AppID: "a", Status: agentcore.RunStatusPaused, PauseReason: agentcore.PauseReasonUserMessage}); err != nil {
			t.Fatal(err)
		}
		if _, err := testEngine(mem, host.NewStaticContextProvider()).ResumeRun(ctx, "a", "r", payload); err == nil {
			t.Fatalf("accepted invalid provenance: %+v", payload)
		}
	}
}
