package main

import "testing"

func TestDispositionRejectsNewOrChangedRunsAndAllowsCompletedRows(t *testing.T) {
	old := runRow{ID: "r1", Scope: "tenant", RuntimeKind: "codex", WorkflowID: "agent-run-r1"}
	if err := validateManifest([]runRow{old}, nil); err != nil {
		t.Fatal(err)
	}
	if err := validateManifest([]runRow{old}, []runRow{old}); err != nil {
		t.Fatal(err)
	}
	changed := old
	changed.WorkflowID = "other"
	for _, rows := range [][]runRow{{changed}, {old, {ID: "r2"}}} {
		if validateManifest([]runRow{old}, rows) == nil {
			t.Fatal("accepted unreviewed workflow")
		}
	}
}
