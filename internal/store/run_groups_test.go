package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type runGroupTestStore interface {
	agentcore.Store
	agentcore.RunGroupStore
}

func runGroupStores(t *testing.T) map[string]runGroupTestStore {
	t.Helper()
	return map[string]runGroupTestStore{"sql": newTestSQLStore(t), "memory": NewMemory()}
}

func newGroupRun(id string) *agentcore.AgentRun {
	return &agentcore.AgentRun{ID: id, AppID: "app-a", AgentID: "agent-a", Target: agentcore.TargetRef{Type: "workspace", ID: "ws-1"}}
}

func createGroupedRun(ctx context.Context, s runGroupTestStore, run *agentcore.AgentRun, groupID string) error {
	return s.CreateRunWithMCP(agentcore.WithRunGroupMembership(ctx, agentcore.RunGroupMembership{GroupID: groupID, ParentRunID: "parent-1"}), run, nil)
}

func setRunStatus(t *testing.T, ctx context.Context, s runGroupTestStore, runID, status string) {
	t.Helper()
	run, err := s.GetRun(ctx, "app-a", runID)
	if err != nil || run == nil {
		t.Fatalf("get run %s: %v", runID, err)
	}
	run.Status = status
	if agentcore.IsTerminalStatus(status) {
		now := time.Now().UTC()
		run.CompletedAt = &now
	}
	if err := s.UpdateRun(ctx, run); err != nil {
		t.Fatalf("update run %s to %s: %v", runID, status, err)
	}
}

func TestRunGroupCreationIsIdempotentByCreationKey(t *testing.T) {
	for name, s := range runGroupStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			first, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a", CreationKey: "plan-7", Policy: agentcore.RunGroupPolicy{MaxActiveRuns: 3}})
			if err != nil {
				t.Fatal(err)
			}
			again, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a", CreationKey: "plan-7"})
			if err != nil || again.ID != first.ID || again.Policy.MaxActiveRuns != 3 {
				t.Fatalf("repeat create = %#v, %v; want %s", again, err, first.ID)
			}
			other, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-b", CreationKey: "plan-7"})
			if err != nil || other.ID == first.ID {
				t.Fatalf("creation keys must be app-scoped: %#v, %v", other, err)
			}
			if first.OwnerKind != agentcore.RunGroupOwnerHost || first.State != agentcore.RunGroupStateActive {
				t.Fatalf("defaults = %#v", first)
			}
			if _, err := s.GetRunGroup(ctx, "app-b", first.ID); !errors.Is(err, agentcore.ErrRunGroupNotFound) {
				t.Fatalf("cross-app get must be not found, got %v", err)
			}
		})
	}
}

func TestRunGroupCompletionSequenceIsAssignedOnceOnFirstTerminalStatus(t *testing.T) {
	for name, s := range runGroupStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			group, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a"})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"run-1", "run-2", "run-3"} {
				if err := createGroupedRun(ctx, s, newGroupRun(id), group.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.CreateRun(ctx, newGroupRun("legacy")); err != nil {
				t.Fatal(err)
			}
			setRunStatus(t, ctx, s, "run-2", agentcore.RunStatusRunning)
			setRunStatus(t, ctx, s, "run-2", agentcore.RunStatusFailed)
			setRunStatus(t, ctx, s, "run-1", agentcore.RunStatusCompleted)
			// A second terminal save (for example cancel racing completion)
			// must not assign another sequence.
			setRunStatus(t, ctx, s, "run-1", agentcore.RunStatusCancelled)
			setRunStatus(t, ctx, s, "legacy", agentcore.RunStatusCompleted)

			page, err := s.ListRunGroupCompletions(ctx, "app-a", group.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 2 || page.NextAfter != 2 {
				t.Fatalf("completions = %#v", page)
			}
			if page.Items[0].Seq != 1 || page.Items[0].RunID != "run-2" || page.Items[0].Status != agentcore.RunStatusFailed {
				t.Fatalf("first completion = %#v", page.Items[0])
			}
			if page.Items[1].Seq != 2 || page.Items[1].RunID != "run-1" || page.Items[1].Status != agentcore.RunStatusCompleted {
				t.Fatalf("second completion keeps first terminal status = %#v", page.Items[1])
			}
			if page.Items[0].CompletedAt.IsZero() {
				t.Fatal("completion time missing")
			}
			next, err := s.ListRunGroupCompletions(ctx, "app-a", group.ID, page.NextAfter, 0)
			if err != nil || len(next.Items) != 0 || next.NextAfter != page.NextAfter {
				t.Fatalf("cursor at head = %#v, %v", next, err)
			}
			limited, err := s.ListRunGroupCompletions(ctx, "app-a", group.ID, 0, 1)
			if err != nil || len(limited.Items) != 1 || limited.NextAfter != 1 {
				t.Fatalf("limited page = %#v, %v", limited, err)
			}
			stored, err := s.GetRunGroup(ctx, "app-a", group.ID)
			if err != nil || stored.LastCompletionSeq != 2 {
				t.Fatalf("group counter = %#v, %v", stored, err)
			}
			member, err := s.GetRunGroupMember(ctx, "app-a", "run-3")
			if err != nil || member == nil || member.CompletionSeq != 0 || member.ParentRunID != "parent-1" || member.Status != agentcore.RunStatusQueued {
				t.Fatalf("non-terminal member = %#v, %v", member, err)
			}
			if legacy, err := s.GetRunGroupMember(ctx, "app-a", "legacy"); err != nil || legacy != nil {
				t.Fatalf("ungrouped run must have no membership: %#v, %v", legacy, err)
			}
		})
	}
}

func TestRunGroupAdmissionRejectsAtomically(t *testing.T) {
	for name, s := range runGroupStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			full, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a", Policy: agentcore.RunGroupPolicy{MaxActiveRuns: 1}})
			if err != nil {
				t.Fatal(err)
			}
			if err := createGroupedRun(ctx, s, newGroupRun("active"), full.ID); err != nil {
				t.Fatal(err)
			}
			if err := createGroupedRun(ctx, s, newGroupRun("over-limit"), full.ID); !errors.Is(err, agentcore.ErrRunGroupFull) {
				t.Fatalf("expected full group, got %v", err)
			}
			setRunStatus(t, ctx, s, "active", agentcore.RunStatusCompleted)
			if err := createGroupedRun(ctx, s, newGroupRun("after-finish"), full.ID); err != nil {
				t.Fatalf("terminal members must free capacity: %v", err)
			}

			paused, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SetRunGroupState(ctx, "app-a", paused.ID, agentcore.RunGroupStatePaused); err != nil {
				t.Fatal(err)
			}
			if err := createGroupedRun(ctx, s, newGroupRun("into-paused"), paused.ID); !errors.Is(err, agentcore.ErrRunGroupClosed) {
				t.Fatalf("expected closed group, got %v", err)
			}
			if err := createGroupedRun(ctx, s, newGroupRun("into-missing"), "missing"); !errors.Is(err, agentcore.ErrRunGroupNotFound) {
				t.Fatalf("expected missing group, got %v", err)
			}
			for _, id := range []string{"over-limit", "into-paused", "into-missing"} {
				if run, err := s.GetRun(ctx, "app-a", id); err != nil || run != nil {
					t.Fatalf("rejected admission must not persist run %s: %#v, %v", id, run, err)
				}
			}
		})
	}
}

func TestRunGroupStateTransitionsAndGeneration(t *testing.T) {
	for name, s := range runGroupStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			group, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a"})
			if err != nil {
				t.Fatal(err)
			}
			if err := createGroupedRun(ctx, s, newGroupRun("run-1"), group.ID); err != nil {
				t.Fatal(err)
			}
			for _, want := range []int64{1, 2} {
				if got, err := s.BumpRunExecutionGeneration(ctx, "app-a", "run-1"); err != nil || got != want {
					t.Fatalf("generation = %d, %v; want %d", got, err, want)
				}
			}
			if _, err := s.BumpRunExecutionGeneration(ctx, "app-a", "not-a-member"); err == nil {
				t.Fatal("bumping a non-member must fail")
			}
			for _, state := range []string{agentcore.RunGroupStatePaused, agentcore.RunGroupStateActive, agentcore.RunGroupStateCancelled} {
				updated, err := s.SetRunGroupState(ctx, "app-a", group.ID, state)
				if err != nil || updated.State != state {
					t.Fatalf("set %s = %#v, %v", state, updated, err)
				}
			}
			if _, err := s.SetRunGroupState(ctx, "app-a", group.ID, agentcore.RunGroupStateActive); !errors.Is(err, agentcore.ErrRunGroupClosed) {
				t.Fatalf("cancelled must be final, got %v", err)
			}
			if _, err := s.SetRunGroupState(ctx, "app-a", group.ID, "bogus"); err == nil {
				t.Fatal("unknown state must be rejected")
			}
		})
	}
}

func TestRunGroupVisibilityDependsOnOwner(t *testing.T) {
	for name, s := range runGroupStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			host, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a"})
			if err != nil {
				t.Fatal(err)
			}
			job, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a", OwnerKind: agentcore.RunGroupOwnerWorkJob})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CreateRun(ctx, newGroupRun("legacy")); err != nil {
				t.Fatal(err)
			}
			if err := createGroupedRun(ctx, s, newGroupRun("host-member"), host.ID); err != nil {
				t.Fatal(err)
			}
			if err := createGroupedRun(ctx, s, newGroupRun("job-worker"), job.ID); err != nil {
				t.Fatal(err)
			}
			runs, err := s.ListRuns(ctx, "app-a")
			if err != nil {
				t.Fatal(err)
			}
			if got := runIDs(runs); !sameIDs(got, "host-member", "legacy") {
				t.Fatalf("default ListRuns = %v", got)
			}
			page, err := s.SearchRuns(ctx, agentcore.RunSearch{AppID: "app-a"})
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 2 || !sameIDs(runIDs(page.Items), "host-member", "legacy") {
				t.Fatalf("default SearchRuns = %v total %d", runIDs(page.Items), page.Total)
			}
			all, err := s.SearchRuns(ctx, agentcore.RunSearch{AppID: "app-a", IncludeHiddenGroupMembers: true})
			if err != nil || all.Total != 3 {
				t.Fatalf("explicit inclusion = %v total %d, %v", runIDs(all.Items), all.Total, err)
			}
			// Internal recovery scans must still see hidden members.
			active, err := s.ListRunsByStatus(ctx, agentcore.RunStatusQueued)
			if err != nil || !sameIDs(runIDs(active), "host-member", "job-worker", "legacy") {
				t.Fatalf("internal scan = %v, %v", runIDs(active), err)
			}
			worker, err := s.GetRunGroupMember(ctx, "app-a", "job-worker")
			if err != nil || worker == nil || !worker.Hidden {
				t.Fatalf("job worker membership = %#v, %v", worker, err)
			}
			members, err := s.ListRunGroupMembers(ctx, "app-a", host.ID)
			if err != nil || len(members) != 1 || members[0].Hidden || members[0].RunID != "host-member" {
				t.Fatalf("host members = %#v, %v", members, err)
			}
		})
	}
}

// TestSQLRunGroupConcurrentTerminalTransitions uses a file-backed database and
// separate goroutines so SQLite's write locking orders the transactions.
func TestSQLRunGroupConcurrentTerminalTransitions(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQL(SQLConfig{Driver: "sqlite", DSN: "file:" + filepath.Join(t.TempDir(), "runtime.db") + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	group, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a"})
	if err != nil {
		t.Fatal(err)
	}
	const members = 24
	for i := 0; i < members; i++ {
		if err := createGroupedRun(ctx, s, newGroupRun(fmt.Sprintf("run-%02d", i)), group.ID); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, members*2)
	for i := 0; i < members; i++ {
		for _, status := range []string{agentcore.RunStatusCompleted, agentcore.RunStatusCancelled} {
			wg.Add(1)
			go func(runID, status string) {
				defer wg.Done()
				run, err := s.GetRun(ctx, "app-a", runID)
				if err != nil {
					errs <- err
					return
				}
				run.Status = status
				errs <- s.UpdateRun(ctx, run)
			}(fmt.Sprintf("run-%02d", i), status)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent terminal update: %v", err)
		}
	}
	page, err := s.ListRunGroupCompletions(ctx, "app-a", group.ID, 0, members+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != members {
		t.Fatalf("each member must complete exactly once: got %d completions", len(page.Items))
	}
	seen := map[string]bool{}
	for i, item := range page.Items {
		if item.Seq != int64(i+1) {
			t.Fatalf("sequences must be gap-free and ordered: item %d has seq %d", i, item.Seq)
		}
		if seen[item.RunID] {
			t.Fatalf("run %s completed twice", item.RunID)
		}
		seen[item.RunID] = true
	}
}

// TestSQLStoreWithoutRunGroupTablesKeepsLegacyBehavior models a store whose
// schema has not been migrated for run groups (the phase 1a Postgres case).
func TestSQLStoreWithoutRunGroupTablesKeepsLegacyBehavior(t *testing.T) {
	ctx := context.Background()
	s := newTestSQLStore(t)
	if err := s.DB().Migrator().DropTable(&runGroupMemberRecord{}, &runGroupRecord{}); err != nil {
		t.Fatal(err)
	}
	s.runGroups = false
	if err := s.CreateRun(ctx, newGroupRun("legacy")); err != nil {
		t.Fatal(err)
	}
	setRunStatus(t, ctx, s, "legacy", agentcore.RunStatusCompleted)
	if runs, err := s.ListRuns(ctx, "app-a"); err != nil || len(runs) != 1 {
		t.Fatalf("legacy listing = %v, %v", runIDs(runs), err)
	}
	if page, err := s.SearchRuns(ctx, agentcore.RunSearch{AppID: "app-a"}); err != nil || page.Total != 1 {
		t.Fatalf("legacy search = %#v, %v", page, err)
	}
	if err := createGroupedRun(ctx, s, newGroupRun("grouped"), "any"); !errors.Is(err, agentcore.ErrRunGroupsUnsupported) {
		t.Fatalf("grouped create must be rejected, got %v", err)
	}
	if run, err := s.GetRun(ctx, "app-a", "grouped"); err != nil || run != nil {
		t.Fatalf("rejected grouped create must not persist: %#v, %v", run, err)
	}
	if _, err := s.CreateRunGroup(ctx, &agentcore.RunGroup{AppID: "app-a"}); !errors.Is(err, agentcore.ErrRunGroupsUnsupported) {
		t.Fatalf("expected unsupported, got %v", err)
	}
	if member, err := s.GetRunGroupMember(ctx, "app-a", "legacy"); err != nil || member != nil {
		t.Fatalf("membership lookup without tables = %#v, %v", member, err)
	}
}

func runIDs(runs []agentcore.AgentRun) []string {
	out := make([]string, 0, len(runs))
	for _, run := range runs {
		out = append(out, run.ID)
	}
	return out
}

func sameIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	set := map[string]int{}
	for _, id := range got {
		set[id]++
	}
	for _, id := range want {
		if set[id] == 0 {
			return false
		}
		set[id]--
	}
	return true
}
