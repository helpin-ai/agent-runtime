package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestNativeStateCASAndIsolation(t *testing.T) {
	for name, db := range map[string]agentcore.NativeStateStore{"memory": NewMemory(), "sql": newTestSQLStore(t)} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := &agentcore.NativeState{AppID: "a", RunID: "r", Payload: json.RawMessage(`{"value":1}`)}
			if err := db.SaveNativeState(ctx, s, []json.RawMessage{json.RawMessage(`{"event":1}`)}); err != nil {
				t.Fatal(err)
			}
			stale := *s
			s.Payload = json.RawMessage(`{"value":2}`)
			if err := db.SaveNativeState(ctx, s, nil); err != nil {
				t.Fatal(err)
			}
			if err := db.SaveNativeState(ctx, &stale, nil); !errors.Is(err, agentcore.ErrNativeStateConflict) {
				t.Fatalf("stale writer: %v", err)
			}
			if stale.Version != 1 {
				t.Fatal("failed CAS advanced version")
			}
			got, err := db.LoadNativeState(ctx, "a", "r")
			if err != nil {
				t.Fatal(err)
			}
			if got.Version != 2 || string(got.Payload) != `{"value":2}` {
				t.Fatalf("checkpoint: %+v", got)
			}
			got.Payload[0] = 'x'
			again, _ := db.LoadNativeState(ctx, "a", "r")
			if !json.Valid(again.Payload) {
				t.Fatal("read aliases stored bytes")
			}
			other, _ := db.LoadNativeState(ctx, "other", "r")
			if other != nil {
				t.Fatal("cross-app read")
			}
			if err := db.SaveNativeState(ctx, s, []json.RawMessage{json.RawMessage(`bad`)}); err == nil {
				t.Fatal("invalid journal accepted")
			}
			again, _ = db.LoadNativeState(ctx, "a", "r")
			if again.Version != 2 {
				t.Fatal("invalid journal changed state")
			}
		})
	}
}

func TestNativeStateSQLJournalFailureRollsBack(t *testing.T) {
	db := newTestSQLStore(t)
	ctx := context.Background()
	s := &agentcore.NativeState{AppID: "a", RunID: "r", Payload: json.RawMessage(`{}`)}
	if err := db.SaveNativeState(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	// Force a journal uniqueness failure after the state update in the transaction.
	if err := db.db.Create(&nativeJournalRecord{AppID: "a", RunID: "r", Version: 2, Position: 0, Payload: jsonBytes(`{}`)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.SaveNativeState(ctx, s, []json.RawMessage{json.RawMessage(`{}`)}); err == nil {
		t.Fatal("expected journal failure")
	}
	got, _ := db.LoadNativeState(ctx, "a", "r")
	if s.Version != 1 || got.Version != 1 {
		t.Fatal("journal failure committed checkpoint")
	}
}
