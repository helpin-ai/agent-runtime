package store

import (
	"context"
	"errors"
	"fmt"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSteeringStoreContract(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var db interface {
				agentcore.Store
				agentcore.SteeringStore
			}
			if kind == "memory" {
				db = NewMemory()
			} else {
				db = openWorkJobSQL(t)
			}
			ctx := context.Background()
			run := &agentcore.AgentRun{AppID: "a", ID: "run", AgentID: "agent", Status: agentcore.RunStatusRunning, RuntimeKind: agentcore.RuntimeNativeSDK}
			if err := db.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			req := sdk.SteerRunRequest{CommandID: "first", Content: "Research only", ExternalActorID: "user", DeniedTools: []string{"crm_write"}}
			first, err := db.CreateRunSteering(ctx, "a", "run", req)
			if err != nil || first.Revision != 1 || first.State != "accepted" {
				t.Fatalf("create: %+v %v", first, err)
			}
			replay, err := db.CreateRunSteering(ctx, "a", "run", req)
			if err != nil || replay.Revision != 1 {
				t.Fatalf("replay: %+v %v", replay, err)
			}
			conflicting := req
			conflicting.Content = "Write CRM"
			if _, err := db.CreateRunSteering(ctx, "a", "run", conflicting); !errors.Is(err, agentcore.ErrSteeringConflict) {
				t.Fatalf("conflicting replay: %v", err)
			}
			req.CommandID = "second"
			if _, err := db.CreateRunSteering(ctx, "a", "run", req); !errors.Is(err, agentcore.ErrSteeringConflict) {
				t.Fatalf("stale revision: %v", err)
			}
			if _, err := db.CreateRunSteering(ctx, "other", "run", req); err == nil {
				t.Fatal("cross tenant create allowed")
			}
			if err := db.ApplyRunSteering(ctx, "other", "run", 1); err != nil {
				t.Fatal(err)
			}
			commands, err := db.ListRunSteering(ctx, "a", "run")
			if err != nil || len(commands) != 1 || commands[0].State != "accepted" {
				t.Fatalf("foreign acknowledgement: %+v %v", commands, err)
			}
			if err := db.ApplyRunSteering(ctx, "a", "run", 1); err != nil {
				t.Fatal(err)
			}
			commands, err = db.ListRunSteering(ctx, "a", "run")
			if err != nil || commands[0].State != "applied" || commands[0].AppliedAt == nil {
				t.Fatalf("acknowledgement: %+v %v", commands, err)
			}
			run.Status = agentcore.RunStatusCompleted
			if err := db.UpdateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			req.ExpectedRevision = 1
			if _, err := db.CreateRunSteering(ctx, "a", "run", req); !errors.Is(err, agentcore.ErrSteeringTerminal) {
				t.Fatalf("terminal create: %v", err)
			}
		})
	}
}

func TestSteeringConcurrentRevisionAndTerminalReceipt(t *testing.T) {
	for _, kind := range []string{"sqlite", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			var db *SQL
			if kind == "postgres" {
				dsn := os.Getenv("AGENT_RUNTIME_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("local PostgreSQL DSN required")
				}
				db = openWorkJobPostgres(t, dsn)
			} else {
				db = openWorkJobSQL(t)
			}
			ctx := context.Background()
			run := &agentcore.AgentRun{AppID: "a", ID: "r", Status: agentcore.RunStatusPaused}
			if err := db.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			var accepted atomic.Int32
			var group sync.WaitGroup
			for i := 0; i < 8; i++ {
				group.Add(1)
				go func(i int) {
					defer group.Done()
					_, err := db.CreateRunSteering(ctx, "a", "r", sdk.SteerRunRequest{CommandID: fmt.Sprintf("c%d", i), Content: "instruction", ExternalActorID: "user"})
					if err == nil {
						accepted.Add(1)
					} else if !errors.Is(err, agentcore.ErrSteeringConflict) {
						t.Errorf("unexpected concurrency error: %v", err)
					}
				}(i)
			}
			group.Wait()
			if accepted.Load() != 1 {
				t.Fatalf("accepted %d conflicting revisions", accepted.Load())
			}
			current, err := db.GetRun(ctx, "a", "r")
			if err != nil || current.Status != agentcore.RunStatusPaused {
				t.Fatal("steering resumed paused run")
			}
			run.Status = agentcore.RunStatusCompleted
			if err := db.UpdateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			commands, err := db.ListRunSteering(ctx, "a", "r")
			if err != nil || len(commands) != 1 || commands[0].State != "not_applied" {
				t.Fatalf("false receipt: %+v %v", commands, err)
			}
		})
	}
}
