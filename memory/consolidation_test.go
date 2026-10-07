package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type consolidationFunc func(context.Context, []Fact, []Observation) (ConsolidationPlan, error)

func (f consolidationFunc) Consolidate(c context.Context, facts []Fact, obs []Observation) (ConsolidationPlan, error) {
	return f(c, facts, obs)
}

func TestConsolidationProvenanceForgetAndRebuild(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	scope := Scope{"app", "bank"}
	for _, id := range []string{"a", "b"} {
		if _, err = s.Retain(ctx, scope, RetainRequest{DocumentID: id, Content: "Alice lives in Paris.", Timestamp: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	model := consolidationFunc(func(_ context.Context, facts []Fact, old []Observation) (ConsolidationPlan, error) {
		ids := []string{}
		for _, f := range facts {
			ids = append(ids, f.ID)
		}
		return ConsolidationPlan{Creates: []ConsolidationAction{{Text: "Alice lives in Paris.", SourceFactIDs: ids}}}, nil
	})
	r, err := s.Consolidate(ctx, scope, model, 100)
	if err != nil || r.Processed != 2 || r.Created != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	obs, err := s.SearchObservations(ctx, scope, "Paris", 10)
	if err != nil || len(obs) != 1 || obs[0].ProofCount != 2 {
		t.Fatalf("%+v %v", obs, err)
	}
	if err = s.Forget(ctx, scope, "a"); err != nil {
		t.Fatal(err)
	}
	obs, err = s.SearchObservations(ctx, scope, "Paris", 10)
	if err != nil || len(obs) != 0 {
		t.Fatalf("forgotten derived text retained: %+v %v", obs, err)
	}
	r, err = s.Consolidate(ctx, scope, model, 100)
	if err != nil || r.Processed != 1 {
		t.Fatalf("surviving proof not eligible for rebuild: %+v %v", r, err)
	}
	obs, err = s.SearchObservations(ctx, scope, "Paris", 10)
	if err != nil || len(obs) != 1 || obs[0].ProofCount != 1 {
		t.Fatalf("%+v %v", obs, err)
	}
}

func TestConsolidationRejectsForeignProofAndStaleInference(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(ctx, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	scope := Scope{"app", "bank"}
	if _, err = s.Retain(ctx, scope, RetainRequest{DocumentID: "a", Content: "Alice lives in Paris."}); err != nil {
		t.Fatal(err)
	}
	bad := consolidationFunc(func(context.Context, []Fact, []Observation) (ConsolidationPlan, error) {
		return ConsolidationPlan{Creates: []ConsolidationAction{{Text: "other bank secret", SourceFactIDs: []string{"foreign"}}}}, nil
	})
	if _, err = s.Consolidate(ctx, scope, bad, 10); err == nil {
		t.Fatal("accepted foreign proof")
	}
	stale := consolidationFunc(func(_ context.Context, f []Fact, _ []Observation) (ConsolidationPlan, error) {
		if err := s.Forget(ctx, scope, "a"); err != nil {
			return ConsolidationPlan{}, err
		}
		return ConsolidationPlan{Creates: []ConsolidationAction{{Text: "Alice lives in Paris.", SourceFactIDs: []string{f[0].ID}}}}, nil
	})
	if _, err = s.Consolidate(ctx, scope, stale, 10); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale inference: %v", err)
	}
	obs, err := s.SearchObservations(ctx, scope, "Paris", 10)
	if err != nil || len(obs) != 0 {
		t.Fatalf("resurrected observation: %+v %v", obs, err)
	}
}

func TestConsolidationSkipsExactDuplicateCreates(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	ctx := context.Background()
	model := consolidationFunc(func(_ context.Context, facts []Fact, existing []Observation) (ConsolidationPlan, error) {
		return ConsolidationPlan{Creates: []ConsolidationAction{{Text: "Alice lives in Paris.", SourceFactIDs: []string{facts[0].ID}}, {Text: "  Alice lives  in Paris. ", SourceFactIDs: []string{facts[0].ID}}}}, nil
	})
	retainTest(t, s, testScope, "first", "Alice lives in Paris.")
	if r, e := s.Consolidate(ctx, testScope, model, 1); e != nil || r.Created != 1 {
		t.Fatalf("%+v %v", r, e)
	}
	retainTest(t, s, testScope, "second", "Alice still lives in Paris.")
	if r, e := s.Consolidate(ctx, testScope, model, 1); e != nil || r.Created != 0 || r.Processed != 1 {
		t.Fatalf("%+v %v", r, e)
	}
}

type adaptiveTestExtractor struct {
	Extractor
	consolidationFunc
}

func TestConsolidationRetriesSmallerBatchAndEnforcesCapacity(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	ctx := context.Background()
	for _, id := range []string{"one", "two", "three", "four"} {
		retainTest(t, s, testScope, id, "Alice lives in Paris.")
	}
	sizes := []int{}
	model := consolidationFunc(func(_ context.Context, f []Fact, _ []Observation) (ConsolidationPlan, error) {
		sizes = append(sizes, len(f))
		if len(f) > 2 {
			return ConsolidationPlan{}, errConsolidationOutput
		}
		ids := []string{}
		for _, fact := range f {
			ids = append(ids, fact.ID)
		}
		return ConsolidationPlan{Creates: []ConsolidationAction{{Text: "Alice lives in Paris.", SourceFactIDs: ids}}}, nil
	})
	s.cfg.Extractor = adaptiveTestExtractor{s.cfg.Extractor, model}
	if r, e := s.ConsolidateMemory(ctx, testScope, 4); e != nil || r.Processed != 2 || len(sizes) != 2 || sizes[0] != 4 || sizes[1] != 2 {
		t.Fatalf("%+v %v sizes=%v", r, e, sizes)
	}
	s.cfg.MaxObservations = 1
	overflow := consolidationFunc(func(_ context.Context, f []Fact, _ []Observation) (ConsolidationPlan, error) {
		return ConsolidationPlan{Creates: []ConsolidationAction{{Text: "A different observation.", SourceFactIDs: []string{f[0].ID}}}}, nil
	})
	if _, e := s.Consolidate(ctx, testScope, overflow, 1); !errors.Is(e, errConsolidationOutput) {
		t.Fatalf("capacity not enforced: %v", e)
	}
	if r, e := s.Consolidate(ctx, testScope, model, 2); e != nil || r.Processed != 2 || r.Created != 0 {
		t.Fatalf("overflow marked facts processed: %+v %v", r, e)
	}
}
