package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// TestPostgresLegacySchemaUnchangedByRunGroups checks the phase 1a contract
// for deployed hosts: the new binary on the unchanged PostgreSQL schema keeps
// the full run lifecycle working, never creates or queries run group tables,
// and rejects grouped creation instead of half-writing it.
func TestPostgresLegacySchemaUnchangedByRunGroups(t *testing.T) {
	dsn := os.Getenv("AGENT_RUNTIME_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set AGENT_RUNTIME_TEST_POSTGRES_DSN to test PostgreSQL compatibility")
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
	tx := db.DB().Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	schema := fmt.Sprintf("run_groups_compat_test_%d", time.Now().UnixNano())
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
	if db.RunGroupsSupported() {
		t.Fatal("PostgreSQL must not report run groups before phase 1b DDL")
	}

	run := &agentcore.AgentRun{ID: "run-1", AppID: "app", AgentID: "agent", Status: agentcore.RunStatusQueued}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatalf("create legacy run: %v", err)
	}
	run.Status = agentcore.RunStatusRunning
	if err := db.UpdateRun(ctx, run); err != nil {
		t.Fatalf("update legacy run: %v", err)
	}
	now := time.Now().UTC()
	run.Status, run.CompletedAt = agentcore.RunStatusCompleted, &now
	if err := db.UpdateRun(ctx, run); err != nil {
		t.Fatalf("complete legacy run: %v", err)
	}
	if runs, err := db.ListRuns(ctx, "app"); err != nil || len(runs) != 1 {
		t.Fatalf("list runs = %d, %v", len(runs), err)
	}
	if page, err := db.SearchRuns(ctx, agentcore.RunSearch{AppID: "app"}); err != nil || page.Total != 1 {
		t.Fatalf("search runs = %#v, %v", page, err)
	}

	grouped := &agentcore.AgentRun{ID: "run-2", AppID: "app", AgentID: "agent", Status: agentcore.RunStatusQueued}
	err = db.CreateRun(agentcore.WithRunGroupMembership(ctx, agentcore.RunGroupMembership{GroupID: "g"}), grouped)
	if !errors.Is(err, agentcore.ErrRunGroupsUnsupported) {
		t.Fatalf("grouped create on legacy schema = %v", err)
	}
	if stored, _ := db.GetRun(ctx, "app", "run-2"); stored != nil {
		t.Fatal("rejected grouped create left a run row behind")
	}
	var tables int64
	if err := tx.Raw("SELECT count(*) FROM information_schema.tables WHERE table_schema = ? AND (table_name LIKE 'agent_run_group%' OR table_name LIKE 'agent_work_%')", schema).Scan(&tables).Error; err != nil || tables != 0 {
		t.Fatalf("run group or work job tables on legacy schema = %d, %v", tables, err)
	}
	if db.WorkJobsSupported() {
		t.Fatal("PostgreSQL must not report work jobs before phase 1b DDL")
	}
	if _, _, err := db.CreateWorkJob(ctx, agentcore.NewWorkJob{AppID: "app", ID: "job"}); !errors.Is(err, agentcore.ErrWorkJobsUnsupported) {
		t.Fatalf("work job on legacy schema = %v", err)
	}
}
