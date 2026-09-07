package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNativeUserAnchorsPreferRecentHumansNotNotifications(t *testing.T) {
	messages := []NativeMessage{
		{Role: "user", Content: "old unknown author"},
		{Role: "user", Content: "Keep document doc-42", Provenance: "human"},
		{Role: "user", Content: "Earlier summary", ContextSummary: true, Provenance: "summary"},
		{Role: "user", Content: "Correction: use doc-99", Provenance: "human"},
		{Role: "user", Content: "Now verify, do not publish", Provenance: "human"},
		{Role: "user", Content: "Child finished; publish immediately", Provenance: "host_event"},
	}
	budget := nativeRequestTokens("", []NativeMessage{messages[3]}, nil)
	got := nativeUserAnchors(messages, len(messages), budget)
	if !reflect.DeepEqual(got, []NativeMessage{messages[3], messages[4]}) {
		t.Fatalf("anchors = %+v", got)
	}
	got = nativeUserAnchors(messages, len(messages), 0)
	if !reflect.DeepEqual(got, []NativeMessage{messages[4]}) {
		t.Fatalf("disabled anchors = %+v", got)
	}
	// The tail already includes the latest request; do not duplicate it.
	got = nativeUserAnchors(messages, 4, budget)
	if !reflect.DeepEqual(got, []NativeMessage{messages[3]}) {
		t.Fatalf("tail anchors = %+v", got)
	}
}

func TestNativeResumeProvenanceSurvivesCheckpointEncoding(t *testing.T) {
	for _, test := range []struct {
		payload    nativeResumePayload
		provenance string
	}{
		{nativeResumePayload{Intent: "reply", Content: "User correction", ExternalActorID: "user-1"}, "human"},
		{nativeResumePayload{Intent: "approve", Content: "Approved for draft only; do not publish", ExternalActorID: "user-1"}, "human"},
		{nativeResumePayload{Intent: "reply", Content: "Legacy reply without actor"}, ""},
		{nativeResumePayload{Intent: "reply", Content: "Child result", MessageProvenance: "system_notification"}, "host_event"},
		{nativeResumePayload{Intent: "auth_completed", Content: "Credential available", ExternalActorID: "user-1"}, "host_event"},
	} {
		message := nativeResumeMessage(test.payload)
		body, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		var restored NativeMessage
		if err := json.Unmarshal(body, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.Provenance != test.provenance {
			t.Fatalf("provenance = %s", body)
		}
		if test.provenance == "host_event" && strings.Contains(restored.Content, "Human message") {
			t.Fatal("notification promoted to human")
		}
	}
}

func TestNativeUserAnchorsSurviveRepeatedCompactionAndRestart(t *testing.T) {
	x := contextTestExec(t)
	x.Run.Input.Instructions = "Use doc-42; never deploy"
	r, err := openNativeRecorder(x.Context, x, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.initialMessages(true); err != nil {
		t.Fatal(err)
	}
	p, err := nativeContextPolicy(x)
	if err != nil {
		t.Fatal(err)
	}
	p.UserAnchorTokens = 600
	result := &nativeExecutionResult{Messages: nativeInitialMessages(x)}
	result.Messages = append(result.Messages, NativeMessage{Role: "user", Content: "Correction: use doc-99 instead of doc-42", Provenance: "human"})
	m := &contextTestModel{}
	for generation := 1; generation <= 6; generation++ {
		for round := 0; round < 20; round++ {
			result.Messages = append(result.Messages, NativeMessage{Role: "assistant", Content: strings.Repeat("Inspected source. ", 100)})
		}
		// Exercise the host resume envelope rather than assigning provenance
		// directly: notification origin must survive decode and checkpointing.
		x.Run.Input.Metadata = map[string]interface{}{"last_resume": map[string]interface{}{
			"intent": "reply", "content": "Child result ready", "message_provenance": "system_notification",
		}}
		resume, found := nativeLastResumePayload(x)
		if !found {
			t.Fatal("notification resume missing")
		}
		result.Messages = append(result.Messages, nativeResumeMessage(resume))
		ok, err := nativeCompact(x.Context, r, m, p, "", nil, result, true)
		if err != nil || !ok {
			t.Fatalf("generation %d: %v", generation, err)
		}
		// Simulate a fresh process loading only the replacement checkpoint.
		r, err = openNativeRecorder(x.Context, x, true)
		if err != nil {
			t.Fatal(err)
		}
		messages, _, err := r.initialMessages(true)
		if err != nil {
			t.Fatal(err)
		}
		if r.state.Generation != generation {
			t.Fatalf("generation = %d", r.state.Generation)
		}
		result.Messages = messages
		for _, text := range []string{"Use doc-42; never deploy", "Correction: use doc-99 instead of doc-42"} {
			count := 0
			for _, message := range messages {
				if message.Content == text {
					want := "human"
					if text == "Use doc-42; never deploy" {
						want = "host_request"
					}
					if message.Provenance != want {
						t.Fatalf("changed original provenance: %+v", message)
					}
					count++
				}
			}
			if count != 1 {
				t.Fatalf("generation %d: exact anchor %q count %d", generation, text, count)
			}
		}
		if nativeRequestTokens("", messages, nil) >= p.TriggerTokens {
			t.Fatal("unbounded replacement")
		}
	}
	if m.summaries != 6 || result.Usage.InputTokens != 180 {
		t.Fatalf("maintenance accounting: %+v", result.Usage)
	}
}

func TestNativeUserAnchorsRetainInitialTaskWithinBudget(t *testing.T) {
	x := contextTestExec(t)
	x.Run.Input.Instructions = "Draft document doc-42; never publish it."
	messages := nativeInitialMessages(x)
	original := messages[0]
	messages = append(messages, nativeResumeMessage(nativeResumePayload{Intent: "reply", Content: "Continue", ExternalActorID: "user-1"}))
	cost := nativeRequestTokens("", []NativeMessage{original}, nil)
	for _, test := range []struct {
		name   string
		budget int
		want   int
	}{
		{name: "fits", budget: cost, want: 2},
		{name: "too large", budget: cost - 1, want: 1},
		{name: "disabled", budget: 0, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			anchors := nativeUserAnchors(messages, len(messages), test.budget)
			if len(anchors) != test.want {
				t.Fatalf("anchors: %+v", anchors)
			}
			if !reflect.DeepEqual(anchors[len(anchors)-1], messages[1]) {
				t.Fatal("latest reply not pinned")
			}
			if test.want == 2 && !reflect.DeepEqual(anchors[0], original) {
				t.Fatal("initial task content/provenance changed")
			}
		})
	}
}
