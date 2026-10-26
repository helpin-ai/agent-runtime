package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestScanRunsByStatusPagesFilteredRuns(t *testing.T) {
	for name, open := range map[string]func(*testing.T) agentcore.Store{
		"memory": func(*testing.T) agentcore.Store { return NewMemory() },
		"sql":    func(t *testing.T) agentcore.Store { return newTestSQLStore(t) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			runStore := open(t)
			create := func(id, status, mode string) {
				t.Helper()
				if err := runStore.CreateRun(ctx, &agentcore.AgentRun{
					ID: id, AppID: "app-a", AgentID: "agent-1",
					Target:      agentcore.TargetRef{Type: "ticket", ID: id},
					RuntimeKind: agentcore.RuntimeNativeSDK, ExecutionMode: mode, Status: status,
				}); err != nil {
					t.Fatalf("create run %s: %v", id, err)
				}
			}
			var want []string
			for i := 0; i < 5; i++ {
				id := fmt.Sprintf("durable-%d", i)
				create(id, agentcore.RunStatusQueued, "durable")
				want = append(want, id)
			}
			create("lightweight-1", agentcore.RunStatusRunning, "lightweight")
			create("done-1", agentcore.RunStatusCompleted, "durable")

			scan := agentcore.RunStatusScan{
				Statuses:      []string{agentcore.RunStatusQueued, agentcore.RunStatusRunning},
				ExecutionMode: "durable",
				UpdatedBefore: time.Now().UTC().Add(time.Minute),
				Limit:         2,
			}
			var got []string
			for pages := 0; ; pages++ {
				if pages > 5 {
					t.Fatalf("scan did not terminate, got %v", got)
				}
				runs, err := runStore.ScanRunsByStatus(ctx, scan)
				if err != nil {
					t.Fatalf("scan runs: %v", err)
				}
				for _, run := range runs {
					got = append(got, run.ID)
				}
				if len(runs) < scan.Limit {
					break
				}
				last := runs[len(runs)-1]
				scan.AfterCreated, scan.AfterID = last.CreatedAt, last.ID
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("scan returned %v, want %v", got, want)
			}

			scan = agentcore.RunStatusScan{
				Statuses:      []string{agentcore.RunStatusQueued},
				UpdatedBefore: time.Now().UTC().Add(-time.Hour),
			}
			runs, err := runStore.ScanRunsByStatus(ctx, scan)
			if err != nil {
				t.Fatalf("scan recent runs: %v", err)
			}
			if len(runs) != 0 {
				t.Fatalf("expected recently updated runs to be skipped, got %d", len(runs))
			}
		})
	}
}
