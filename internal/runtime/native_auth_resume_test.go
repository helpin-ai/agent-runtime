package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestNativeAuthenticationResumePreservesPendingReplyOnce(t *testing.T) {
	for _, consumed := range []bool{false, true} {
		for _, notification := range []bool{false, true} {
			t.Run(fmt.Sprintf("consumed=%v/notification=%v", consumed, notification), func(t *testing.T) {
				x := contextTestExec(t)
				x.Agent.ExecutionConfig = json.RawMessage(`{}`)
				model := &contextTestModel{}
				config := NativeConfig{ModelFactory: fakeNativeFactory{model: model}}
				if _, err := executeNativeModel(x.Context, x, config); err != nil {
					t.Fatal(err)
				}
				question := "Which other English prospects are in the list?"
				reply := map[string]any{"intent": "reply", "content": question, "resume_id": "human-1", "message_provenance": "human", "external_actor_id": "user-1"}
				wantProvenance := "human"
				if notification {
					reply["message_provenance"], reply["external_actor_id"] = "system_notification", ""
					wantProvenance = "host_event"
				}
				x.Run.Input.Metadata = map[string]any{"last_resume": reply}
				if consumed {
					if _, err := executeNativeModel(x.Context, x, config); err != nil {
						t.Fatal(err)
					}
					// Older checkpoints know only the last control-resume key.
					stateStore := x.Store.(agentcore.NativeStateStore)
					record, err := stateStore.LoadNativeState(x.Context, x.AppID, x.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					var state map[string]json.RawMessage
					if err := json.Unmarshal(record.Payload, &state); err != nil {
						t.Fatal(err)
					}
					delete(state, "applied_resume_key")
					record.Payload, _ = json.Marshal(state)
					if err := stateStore.SaveNativeState(x.Context, record, nil); err != nil {
						t.Fatal(err)
					}
				}
				for refresh := 1; refresh <= 2; refresh++ {
					x.Run.Input.Metadata["last_resume"] = map[string]any{"intent": "auth_completed", "resume_id": fmt.Sprint("auth-", refresh), "interrupted_resume": reply}
					if _, err := executeNativeModel(x.Context, x, config); err != nil {
						t.Fatal(err)
					}
					count := 0
					for _, message := range model.requests[len(model.requests)-1].Messages {
						if message.Role == "user" && strings.Contains(message.Content, question) {
							count++
							if message.Provenance != wantProvenance {
								t.Fatalf("reply provenance changed: %+v", message)
							}
						}
					}
					if count != 1 {
						t.Fatalf("refresh %d: model received the pending reply %d times", refresh, count)
					}
					requests := len(model.requests)
					if _, err := executeNativeModel(x.Context, x, config); err != nil {
						t.Fatal(err)
					}
					if len(model.requests) != requests {
						t.Fatal("authentication resume replay resampled the completed turn")
					}
				}
			})
		}
	}
}
