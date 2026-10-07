package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type reconcilingModel struct {
	consolidationFunc
	reconcile func(context.Context, string, string) (ObservationReconciliation, error)
}

func (m reconcilingModel) ReconcileObservations(c context.Context, a, b string) (ObservationReconciliation, error) {
	return m.reconcile(c, a, b)
}

func TestConsolidationSemanticFoldPreservesProofsAndForget(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	model := reconcilingModel{consolidationFunc: consolidationFunc(func(_ context.Context, f []Fact, existing []Observation) (ConsolidationPlan, error) {
		text := "Alice lives in Paris."
		if len(existing) > 0 {
			text = "Alice still lives in Paris."
		}
		return ConsolidationPlan{Creates: []ConsolidationAction{{Text: text, SourceFactIDs: []string{f[0].ID}}}}, nil
	}), reconcile: func(_ context.Context, a, b string) (ObservationReconciliation, error) {
		if a != "Alice still lives in Paris." || b != "Alice lives in Paris." {
			t.Fatalf("unexpected verdict evidence: %q / %q", a, b)
		}
		return ObservationReconciliation{Action: "merge", Text: a}, nil
	}}
	retainTest(t, s, testScope, "first", "Alice moved to Paris.")
	if r, e := s.Consolidate(ctx, testScope, model, 1); e != nil || r.Created != 1 {
		t.Fatalf("%+v %v", r, e)
	}
	retainTest(t, s, testScope, "second", "Alice still lives in Paris.")
	if r, e := s.Consolidate(ctx, testScope, model, 1); e != nil || r.Created != 0 || r.Updated != 1 {
		t.Fatalf("duplicate was not folded: %+v %v", r, e)
	}
	observations, e := s.SearchObservations(ctx, testScope, "Paris", 10)
	if e != nil || len(observations) != 1 || observations[0].ProofCount != 2 {
		t.Fatalf("proofs lost: %+v %v", observations, e)
	}
	if e = s.Forget(ctx, testScope, "first"); e != nil {
		t.Fatal(e)
	}
	observations, e = s.SearchObservations(ctx, testScope, "Paris", 10)
	if e != nil || len(observations) != 0 {
		t.Fatalf("folded forgotten evidence survived: %+v %v", observations, e)
	}
}

func TestConsolidationDedupFailureDoesNotMarkSourcesProcessed(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	create := consolidationFunc(func(_ context.Context, f []Fact, existing []Observation) (ConsolidationPlan, error) {
		text := "Paris"
		if len(existing) > 0 {
			text = "Paris."
		}
		return ConsolidationPlan{Creates: []ConsolidationAction{{Text: text, SourceFactIDs: []string{f[0].ID}}}}, nil
	})
	retainTest(t, s, testScope, "first", "Paris")
	if _, e := s.Consolidate(ctx, testScope, create, 1); e != nil {
		t.Fatal(e)
	}
	retainTest(t, s, testScope, "second", "Paris")
	failed := reconcilingModel{consolidationFunc: create, reconcile: func(context.Context, string, string) (ObservationReconciliation, error) {
		return ObservationReconciliation{}, errors.New("local model unavailable")
	}}
	if _, e := s.Consolidate(ctx, testScope, failed, 1); e == nil {
		t.Fatal("dedup transport failure accepted")
	}
	model := reconcilingModel{consolidationFunc: create, reconcile: func(context.Context, string, string) (ObservationReconciliation, error) {
		return ObservationReconciliation{Action: "keep"}, nil
	}}
	if r, e := s.Consolidate(ctx, testScope, model, 1); e != nil || r.Processed != 1 || r.Created != 1 {
		t.Fatalf("failed input could not retry: %+v %v", r, e)
	}
}
