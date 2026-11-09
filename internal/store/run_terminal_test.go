package store

import (
	"context"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"os"
	"testing"
)

func TestStaleRunUpdateCannotReopenTerminalRun(t *testing.T) {
	stores := map[string]agentcore.Store{"memory": NewMemory(), "sql": newTestSQLStore(t)}
	if dsn := os.Getenv("AGENT_RUNTIME_TEST_POSTGRES_DSN"); dsn != "" {
		stores["postgres"] = openWorkJobPostgres(t, dsn)
	}
	for name, db := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, terminal := range []string{agentcore.RunStatusCancelled, agentcore.RunStatusCompleted, agentcore.RunStatusFailed} {
				run := &agentcore.AgentRun{AppID: "app", ID: terminal, Status: agentcore.RunStatusRunning}
				if err := db.CreateRun(ctx, run); err != nil {
					t.Fatal(err)
				}
				stale := *run
				run.Status = terminal
				if err := db.UpdateRun(ctx, run); err != nil {
					t.Fatal(err)
				}
				if err := db.UpdateRun(ctx, &stale); err == nil {
					t.Error("stale running update accepted")
				}
				got, err := db.GetRun(ctx, "app", terminal)
				if err != nil || got.Status != terminal {
					t.Fatalf("terminal run reopened: %v %v", got, err)
				}
				if err := db.UpdateRun(ctx, got); err != nil {
					t.Fatalf("same terminal state update rejected: %v", err)
				}
			}
		})
	}
}
