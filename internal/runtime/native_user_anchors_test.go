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
	result := &nativeExecutionResult{Messages: []NativeMessage{
		{Role: "user", Content: "Use doc-42; never deploy", Provenance: "human"},
		{Role: "user", Content: "Correction: use doc-99 instead of doc-42", Provenance: "human"},
	}}
	m := &contextTestModel{}
	for generation := 1; generation <= 6; generation++ {
		for round := 0; round < 20; round++ {
			result.Messages = append(result.Messages, NativeMessage{Role: "assistant", Content: strings.Repeat("Inspected source. ", 100)})
		}
		result.Messages = append(result.Messages, NativeMessage{Role: "user", Content: "Child result ready", Provenance: "host_event"})
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
				if message.Provenance == "human" && message.Content == text {
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
