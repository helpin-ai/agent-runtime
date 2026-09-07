package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestMigratePostgresNativeCheckpoints(t *testing.T) {
	dsn := os.Getenv("AGENT_RUNTIME_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set AGENT_RUNTIME_TEST_POSTGRES_DSN to test PostgreSQL migrations")
	}
	db, err := OpenSQL(SQLConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB().DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Isolate all DDL and data in a temporary schema and roll it back after the
	// test. Use the production PostgreSQL migration path, never AutoMigrate.
	tx := db.DB().Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	schema := fmt.Sprintf("native_migration_test_%d", time.Now().UnixNano())
	if err := tx.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec("SET LOCAL search_path TO " + schema).Error; err != nil {
		t.Fatal(err)
	}
	db = NewSQL(tx)
	ctx := context.Background()
	if err := db.MigratePostgres(ctx); err != nil {
		t.Fatalf("migrate PostgreSQL: %v", err)
	}
	state, err := db.LoadNativeState(ctx, "app", "run")
	if err != nil || state != nil {
		t.Fatalf("load new run checkpoint: state=%+v, err=%v", state, err)
	}
	state = &agentcore.NativeState{AppID: "app", RunID: "run", Payload: json.RawMessage(`{"format":1}`)}
	journal := []json.RawMessage{json.RawMessage(`{"kind":"execution_start"}`)}
	if err := db.SaveNativeState(ctx, state, journal); err != nil {
		t.Fatalf("save checkpoint and journal: %v", err)
	}
	// Both API and worker run migrations on startup. Repeating the migration
	// must preserve existing checkpoints and journal entries.
	if err := db.MigratePostgres(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	assertCheckpoint := func(version int64, format int) {
		t.Helper()
		got, err := db.LoadNativeState(ctx, "app", "run")
		if err != nil || got == nil || got.Version != version {
			t.Fatalf("checkpoint: state=%+v, err=%v", got, err)
		}
		var payload struct {
			Format int `json:"format"`
		}
		if err := json.Unmarshal(got.Payload, &payload); err != nil || payload.Format != format {
			t.Fatalf("checkpoint payload: %s, err=%v", got.Payload, err)
		}
	}
	assertCheckpoint(1, 1)
	var records []nativeJournalRecord
	if err := tx.Where("app_id = ? AND run_id = ?", "app", "run").Find(&records).Error; err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Version != 1 || records[0].Position != 0 {
		t.Fatalf("journal after migration: %+v", records)
	}
	var entry struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(records[0].Payload, &entry); err != nil || entry.Kind != "execution_start" {
		t.Fatalf("journal payload: %s, err=%v", records[0].Payload, err)
	}
	stale := *state
	state.Payload = json.RawMessage(`{"format":2}`)
	if err := db.SaveNativeState(ctx, state, journal); err != nil {
		t.Fatalf("advance checkpoint: %v", err)
	}
	if err := db.SaveNativeState(ctx, &stale, nil); !errors.Is(err, agentcore.ErrNativeStateConflict) {
		t.Fatalf("stale checkpoint write: %v", err)
	}
	// Identical run IDs in different apps must have independent checkpoints.
	other := &agentcore.NativeState{AppID: "other", RunID: "run", Payload: json.RawMessage(`{}`)}
	if err := db.SaveNativeState(ctx, other, journal); err != nil {
		t.Fatalf("save another app's checkpoint: %v", err)
	}
	assertCheckpoint(2, 2)
}
