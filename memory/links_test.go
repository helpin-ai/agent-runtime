package memory

import (
	"context"
	"math"
	"path/filepath"
	"testing"
)

func TestSQLiteSemanticLinksDiscoverNonSeedInEitherDirection(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
		cfg.Extractor = extractorFunc(func(_ context.Context, r RetainRequest) ([]ExtractedFact, error) {
			return []ExtractedFact{{Text: r.Content, Type: "world"}}, nil
		})
		cfg.Embedder = embedder{id: "test", vectors: map[string][]float32{"query": {1, 0}, "seed": {0.4, 0.9165151}, "neighbor": {0, 1}}}
		s := openTest(t, cfg)
		if reverse {
			retainTest(t, s, testScope, "neighbor", "neighbor")
			retainTest(t, s, testScope, "seed", "seed")
		} else {
			retainTest(t, s, testScope, "seed", "seed")
			retainTest(t, s, testScope, "neighbor", "neighbor")
		}
		result := recallTest(t, s, testScope, "query")
		if len(result.Results) != 2 {
			t.Fatalf("reverse=%v: semantic discovery missing: %+v", reverse, result)
		}
		for _, fact := range result.Results {
			if fact.DocumentID == "neighbor" && (fact.SourceRanks["graph"] == 0 || fact.SourceRanks["semantic"] != 0) {
				t.Fatalf("reverse=%v: wrong discovery provenance: %+v", reverse, fact)
			}
		}
		if err := s.Forget(context.Background(), testScope, "neighbor"); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM memory_links`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("forgotten graph edges survived: count=%d err=%v", count, err)
		}
	}
}

func TestGraphSignalsAddAndCausalTraversalIsDirected(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "seed", "seed")
	retainTest(t, s, testScope, "target", "target")
	var seed, target string
	s.db.QueryRow(`SELECT id FROM memory_facts WHERE document_id='seed'`).Scan(&seed)
	s.db.QueryRow(`SELECT id FROM memory_facts WHERE document_id='target'`).Scan(&target)
	if _, err := s.db.Exec(`INSERT INTO memory_links(source_id,target_id,kind,weight) VALUES(?,?,'caused_by',1)`, seed, target); err != nil {
		t.Fatal(err)
	}
	facts := map[string]candidate{seed: {fact: Fact{ID: seed, Type: "world", Entities: []string{"Alice"}}}, target: {fact: Fact{ID: target, Type: "world", Entities: []string{"Alice"}}}}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result, err := graphCandidates(context.Background(), tx, testScope, "", facts, []ranked{{seed, 0.4}}, nil, 100)
	if err != nil || len(result) != 1 || math.Abs(result[0].score-(math.Tanh(0.5)+2)) > 1e-9 {
		t.Fatalf("signals did not add: %+v %v", result, err)
	}
	// Existing semantic edge traverses both ways; causal evidence adds only
	// when following the stored source -> target direction.
	result, err = graphCandidates(context.Background(), tx, testScope, "", facts, []ranked{{target, 0.4}}, nil, 100)
	if err != nil || len(result) != 1 || math.Abs(result[0].score-(math.Tanh(0.5)+1)) > 1e-9 {
		t.Fatalf("causal traversed backwards: %+v %v", result, err)
	}
}

func TestSQLiteMigratesVersionOneLinkWeights(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "source", "source")
	retainTest(t, s, testScope, "target", "target")
	if _, err := s.db.Exec(`CREATE TABLE old_links (source_id TEXT NOT NULL REFERENCES memory_facts(id) ON DELETE CASCADE,target_id TEXT NOT NULL REFERENCES memory_facts(id) ON DELETE CASCADE,kind TEXT NOT NULL,PRIMARY KEY(source_id,target_id,kind))`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO old_links SELECT source_id,target_id,'caused_by' FROM memory_links LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE memory_links`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`ALTER TABLE old_links RENAME TO memory_links`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE memory_schema SET version=1`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	migrated := openTest(t, cfg)
	var version int
	var weight float64
	if err := migrated.db.QueryRowContext(ctx, `SELECT version FROM memory_schema`).Scan(&version); err != nil || version != 6 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := migrated.db.QueryRowContext(ctx, `SELECT weight FROM memory_links`).Scan(&weight); err != nil || weight != 1 {
		t.Fatalf("weight=%v err=%v", weight, err)
	}
	if len(recallTest(t, migrated, testScope, "source").Results) != 2 {
		t.Fatal("migration lost retained facts")
	}
}
