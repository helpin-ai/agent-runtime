package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestRecoverInterruptedLightweightRunsSkipsWorkJobWorkers(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	hostGroup, err := mem.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a"})
	if err != nil {
		t.Fatal(err)
	}
	jobGroup, err := mem.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a", OwnerKind: agentcore.RunGroupOwnerWorkJob})
	if err != nil {
		t.Fatal(err)
	}
	create := func(id, groupID string) {
		t.Helper()
		run := &agentcore.AgentRun{ID: id, AppID: "app-a", AgentID: "missing", ExecutionMode: ExecutionModeLightweight, Status: agentcore.RunStatusRunning}
		if err := mem.CreateRun(agentcore.WithRunGroupMembership(ctx, agentcore.RunGroupMembership{GroupID: groupID}), run); err != nil {
			t.Fatal(err)
		}
	}
	create("ordinary", "")
	create("host-member", hostGroup.ID)
	create("job-worker", jobGroup.ID)

	// Recovery ownership does not depend on the app opting in: a job worker
	// is never recovered generically, even with run groups disabled.
	count, err := New(Config{Store: mem}).RecoverInterruptedLightweightRuns(ctx)
	if err != nil || count != 2 {
		t.Fatalf("want ordinary and host-member recovered, got %d, %v", count, err)
	}
	waitForEngineStatus(t, mem, "ordinary", agentcore.RunStatusFailed)
	waitForEngineStatus(t, mem, "host-member", agentcore.RunStatusFailed)
	if stored, _ := mem.GetRun(ctx, "app-a", "job-worker"); stored.Status != agentcore.RunStatusRunning {
		t.Fatalf("job worker must be left for the job reconciler, got %s", stored.Status)
	}
}

func TestStartRunGroupAdmissionRequiresOptInAndHostOwnedGroup(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	jobGroup, err := mem.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a", OwnerKind: agentcore.RunGroupOwnerWorkJob})
	if err != nil {
		t.Fatal(err)
	}
	disabled := New(Config{Store: mem})
	req := StartRunRequest{AppID: "app-a", AgentID: "agent-a", Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"}, GroupID: jobGroup.ID}
	if _, err := disabled.StartRun(ctx, req); !errors.Is(err, ErrRunGroupsDisabled) {
		t.Fatalf("disabled app must be rejected, got %v", err)
	}
	enabled := New(Config{Store: mem, RunGroupsEnabled: func(appID string) bool { return appID == "app-a" }})
	if _, err := enabled.StartRun(ctx, req); !errors.Is(err, agentcore.ErrRunGroupNotFound) {
		t.Fatalf("hosts must not start runs in work-job groups, got %v", err)
	}
	req.GroupID = ""
	req.ParentRunID = "parent"
	if _, err := enabled.StartRun(ctx, req); err == nil {
		t.Fatal("parent_run_id without group_id must be rejected")
	}
	if _, err := enabled.CreateRunGroup(ctx, agentcore.RunGroup{AppID: "app-b"}); !errors.Is(err, ErrRunGroupsDisabled) {
		t.Fatalf("app-b must be rejected, got %v", err)
	}
	group, err := enabled.CreateRunGroup(ctx, agentcore.RunGroup{AppID: "app-a", OwnerKind: agentcore.RunGroupOwnerWorkJob})
	if err != nil || group.OwnerKind != agentcore.RunGroupOwnerHost {
		t.Fatalf("host entry point must create host-owned groups: %#v, %v", group, err)
	}
	if ok, reason := enabled.RunGroupsAvailable("app-a"); !ok || reason != "" {
		t.Fatalf("availability = %v %q", ok, reason)
	}
	if ok, reason := enabled.RunGroupsAvailable("app-b"); ok || reason != RunGroupsReasonAppDisabled {
		t.Fatalf("availability = %v %q", ok, reason)
	}
}

func TestCancelRunGroupCancelsActiveMembersAndClosesAdmission(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	eng := New(Config{Store: mem, RunGroupsEnabled: func(string) bool { return true }})
	group, err := eng.CreateRunGroup(ctx, agentcore.RunGroup{AppID: "app-a"})
	if err != nil {
		t.Fatal(err)
	}
	member := func(id, status string) {
		t.Helper()
		run := &agentcore.AgentRun{ID: id, AppID: "app-a", AgentID: "agent-a", ExecutionMode: ExecutionModeLightweight, Status: status}
		if err := mem.CreateRun(agentcore.WithRunGroupMembership(ctx, agentcore.RunGroupMembership{GroupID: group.ID}), run); err != nil {
			t.Fatal(err)
		}
	}
	member("active", agentcore.RunStatusRunning)
	member("done", agentcore.RunStatusQueued)
	done, _ := mem.GetRun(ctx, "app-a", "done")
	done.Status = agentcore.RunStatusCompleted
	if err := mem.UpdateRun(ctx, done); err != nil {
		t.Fatal(err)
	}

	cancelled, err := eng.CancelRunGroup(ctx, "app-a", group.ID)
	if err != nil || cancelled.State != agentcore.RunGroupStateCancelled {
		t.Fatalf("cancel group = %#v, %v", cancelled, err)
	}
	if stored, _ := mem.GetRun(ctx, "app-a", "active"); stored.Status != agentcore.RunStatusCancelled {
		t.Fatalf("active member status = %s", stored.Status)
	}
	if stored, _ := mem.GetRun(ctx, "app-a", "done"); stored.Status != agentcore.RunStatusCompleted {
		t.Fatalf("terminal member must be untouched, got %s", stored.Status)
	}
	page, err := eng.ListRunGroupCompletions(ctx, "app-a", group.ID, 0, 0)
	if err != nil || len(page.Items) != 2 || page.Items[1].RunID != "active" || page.Items[1].Status != agentcore.RunStatusCancelled {
		t.Fatalf("completions = %#v, %v", page, err)
	}
	run := &agentcore.AgentRun{ID: "late", AppID: "app-a", AgentID: "agent-a"}
	if err := mem.CreateRun(agentcore.WithRunGroupMembership(ctx, agentcore.RunGroupMembership{GroupID: group.ID}), run); !errors.Is(err, agentcore.ErrRunGroupClosed) {
		t.Fatalf("cancelled group must reject members, got %v", err)
	}
	if _, err := eng.ResumeRunGroup(ctx, "app-a", group.ID); !errors.Is(err, agentcore.ErrRunGroupClosed) {
		t.Fatalf("cancelled group must not resume, got %v", err)
	}
}

func waitForEngineStatus(t *testing.T, mem *store.Memory, runID, status string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, _ := mem.GetRun(context.Background(), "app-a", runID)
		if stored != nil && stored.Status == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach %s", runID, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
