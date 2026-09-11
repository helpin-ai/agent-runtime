package agentcore

import (
	"testing"
	"time"
)

func TestTurnIdentitySurvivesRetriesAndChangesForAcceptedResume(t *testing.T) {
	run := &AgentRun{AppID: "helpin", ID: "run", Input: RunInput{TurnPolicy: TurnPolicy{CompletionMode: TurnCompletionExplicit}}}
	initial := TurnIdentity(run)
	now := time.Now()
	run.StartedAt = &now
	if TurnIdentity(run) != initial {
		t.Fatal("worker attempt changed turn identity")
	}
	run.Input.Metadata = map[string]interface{}{"last_resume": map[string]interface{}{"resume_id": "reply-1"}}
	resumed := TurnIdentity(run)
	if resumed == initial {
		t.Fatal("accepted reply reused initial turn")
	}
	if TurnIdentity(run) != resumed {
		t.Fatal("retry changed resumed turn")
	}
	run.Input.Metadata["last_resume"] = map[string]interface{}{"resume_id": "interaction-2"}
	if TurnIdentity(run) == resumed {
		t.Fatal("interaction reused previous turn")
	}
}

func TestExplicitTurnMetadataDoesNotInferCompletionFromProse(t *testing.T) {
	run := &AgentRun{AppID: "helpin", ID: "run", Input: RunInput{TurnPolicy: TurnPolicy{CompletionMode: TurnCompletionExplicit}}}
	for _, prose := range []string{"Done", "Complete", "Here is the full answer:", "anything"} {
		data := map[string]interface{}{"content": prose}
		out := WithTurnEventMetadata(run, "assistant_message_completed", data)
		if out["message_type"] != "assistant_progress" || out["turn_id"] == "" {
			t.Fatalf("incorrect progress contract: %+v", out)
		}
		if data["message_type"] != nil {
			t.Fatal("mutated caller payload")
		}
	}
	out := WithTurnEventMetadata(run, "assistant_message_completed", map[string]interface{}{"message_type": "assistant_final"})
	if out["message_type"] != "assistant_final" {
		t.Fatal("lost canonical final")
	}
	run.Input.TurnPolicy.CompletionMode = ""
	out = WithTurnEventMetadata(run, "assistant_message_completed", map[string]interface{}{"content": "legacy"})
	if out["turn_id"] != nil || out["message_type"] != nil {
		t.Fatal("changed legacy contract")
	}
}
