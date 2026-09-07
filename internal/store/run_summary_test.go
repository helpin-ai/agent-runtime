package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestRunSummaryPreservesCancellation(t *testing.T) {
	for name, db := range map[string]interface {
		agentcore.Store
		agentcore.RunSummaryStore
	}{"memory": NewMemory(), "sql": newTestSQLStore(t)} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			run := &agentcore.AgentRun{AppID: "app", ID: "run", Status: agentcore.RunStatusCancelled}
			if err := db.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			if err := db.UpdateRunOutputSummary(ctx, "app", "run", json.RawMessage(`{"reconciled":true}`)); err != nil {
				t.Fatal(err)
			}
			got, err := db.GetRun(ctx, "app", "run")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != agentcore.RunStatusCancelled || string(got.OutputSummary) != `{"reconciled":true}` {
				t.Fatalf("summary write changed lifecycle: %+v", got)
			}
			if err := db.UpdateRunOutputSummary(ctx, "other", "run", json.RawMessage(`{}`)); err == nil {
				t.Fatal("cross-app update accepted")
			}
		})
	}
}
