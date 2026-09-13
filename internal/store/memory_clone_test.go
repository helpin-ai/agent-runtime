package store

import (
	"context"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// The engine mutates run input metadata on a background goroutine while an
// API handler may still be encoding the run it was handed. Every copy the
// memory store hands out must therefore own its metadata map.
func TestMemoryRunCopiesDoNotShareMetadata(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	run := &agentcore.AgentRun{AppID: "app", ID: "run-1", Input: agentcore.RunInput{Metadata: map[string]interface{}{"native_coding": false}}}
	if err := m.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	run.Input.Metadata["caller"] = "mutated after create"

	got, err := m.GetRun(ctx, "app", "run-1")
	if err != nil || got == nil {
		t.Fatalf("get run: %v", err)
	}
	if _, ok := got.Input.Metadata["caller"]; ok {
		t.Fatal("stored run shares the caller's metadata map")
	}
	got.Input.Metadata["turn_started_at"] = "now"

	again, err := m.GetRun(ctx, "app", "run-1")
	if err != nil || again == nil {
		t.Fatalf("get run again: %v", err)
	}
	if _, ok := again.Input.Metadata["turn_started_at"]; ok {
		t.Fatal("returned run shares the stored metadata map")
	}
	list, err := m.ListRuns(ctx, "app")
	if err != nil || len(list) != 1 {
		t.Fatalf("list runs: %v (%d)", err, len(list))
	}
	list[0].Input.Metadata["listed"] = true
	if final, _ := m.GetRun(ctx, "app", "run-1"); final.Input.Metadata["listed"] != nil {
		t.Fatal("listed run shares the stored metadata map")
	}
}
