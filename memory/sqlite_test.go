package memory

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type extractorFunc func(context.Context, RetainRequest) ([]ExtractedFact, error)

func (f extractorFunc) Extract(ctx context.Context, r RetainRequest) ([]ExtractedFact, error) {
	return f(ctx, r)
}

type embedder struct {
	id      string
	vectors map[string][]float32
}

func (e embedder) ModelID() string { return e.id }
func (e embedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = e.vectors[text]
		if out[i] == nil {
			out[i] = []float32{1, 0}
		}
	}
	return out, nil
}

type rerankFunc func(context.Context, string, []string) ([]float64, error)

func (f rerankFunc) Rerank(ctx context.Context, q string, docs []string) ([]float64, error) {
	return f(ctx, q, docs)
}

var testScope = Scope{AppID: "app", BankID: "user"}

func testConfig(path string) SQLiteConfig {
	return SQLiteConfig{Path: path, Extractor: extractorFunc(func(_ context.Context, r RetainRequest) ([]ExtractedFact, error) {
		return []ExtractedFact{{Text: r.Content, Type: "world", Entities: []string{"Alice"}}}, nil
	}), Embedder: embedder{id: "test-embedding-v1"}}
}
func openTest(t *testing.T, cfg SQLiteConfig) *SQLite {
	t.Helper()
	s, err := OpenSQLite(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func retainTest(t *testing.T, s *SQLite, scope Scope, id, text string) RetainResult {
	t.Helper()
	r, e := s.Retain(context.Background(), scope, RetainRequest{DocumentID: id, Content: text})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func recallTest(t *testing.T, s *SQLite, scope Scope, q string) RecallResult {
	t.Helper()
	r, e := s.Recall(context.Background(), scope, RecallRequest{Query: q, MaxTokens: 32768, QueryTimestamp: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestSQLiteReplaceIdempotencyIsolationAndForget(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	retainTest(t, s, testScope, "chat", "Alice works at Acme")
	if r := retainTest(t, s, testScope, "chat", "Alice works at Acme"); !r.Unchanged || r.FactCount != 1 {
		t.Fatalf("retry: %+v", r)
	}
	retainTest(t, s, testScope, "chat", "Alice now works at Beta")
	beforeOtherBanks := recallTest(t, s, testScope, "Alice")
	other := Scope{AppID: "other-app", BankID: "user"}
	retainTest(t, s, other, "chat", "private other-app fact")
	otherBank := Scope{AppID: "app", BankID: "other-user"}
	retainTest(t, s, otherBank, "chat", "private other-user fact")
	r := recallTest(t, s, testScope, "Alice")
	if !reflect.DeepEqual(beforeOtherBanks, r) {
		t.Fatal("other banks changed retrieval ordering or scores")
	}
	if len(r.Results) != 1 || r.Results[0].Text != "Alice now works at Beta" {
		t.Fatalf("replacement/isolation: %+v", r)
	}
	// Replacement must remove obsolete FTS rows, not only vector records.
	var count int
	fts := ftsTable(testScope)
	if err := s.db.QueryRow(`SELECT count(*) FROM ` + fts + ` WHERE ` + fts + ` MATCH 'Acme'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("obsolete keyword row: %d %v", count, err)
	}
	if err := s.Forget(context.Background(), testScope, "chat"); err != nil {
		t.Fatal(err)
	}
	if len(recallTest(t, s, testScope, "Alice").Results) != 0 {
		t.Fatal("forgotten fact returned")
	}
	if len(recallTest(t, s, other, "private").Results) != 1 || len(recallTest(t, s, otherBank, "private").Results) != 1 {
		t.Fatal("forget crossed scope")
	}
	_, err := s.Retain(context.Background(), testScope, RetainRequest{DocumentID: "chat", Content: "resurrection"})
	if !errors.Is(err, ErrForgotten) {
		t.Fatalf("expected forgotten: %v", err)
	}
}

func TestSQLiteForgetWinsAgainstInflightExtraction(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	cfg.Extractor = extractorFunc(func(_ context.Context, r RetainRequest) ([]ExtractedFact, error) {
		close(started)
		<-release
		return []ExtractedFact{{Text: r.Content, Type: "world"}}, nil
	})
	s := openTest(t, cfg)
	done := make(chan error, 1)
	go func() {
		_, err := s.Retain(context.Background(), testScope, RetainRequest{DocumentID: "chat", Content: "secret"})
		done <- err
	}()
	<-started
	if err := s.Forget(context.Background(), testScope, "chat"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrForgotten) {
		t.Fatalf("forget lost race: %v", err)
	}
	if len(recallTest(t, s, testScope, "secret").Results) != 0 {
		t.Fatal("secret resurrected")
	}
}

func TestSQLiteFailedReplacementPreservesEvidence(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	cfg.Extractor = extractorFunc(func(_ context.Context, r RetainRequest) ([]ExtractedFact, error) {
		if r.Content == "bad" {
			return nil, errors.New("provider failure")
		}
		return []ExtractedFact{{Text: r.Content, Type: "world"}}, nil
	})
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "chat", "original")
	if _, err := s.Retain(context.Background(), testScope, RetainRequest{DocumentID: "chat", Content: "bad"}); err == nil {
		t.Fatal("expected failure")
	}
	r := recallTest(t, s, testScope, "original")
	if len(r.Results) != 1 || r.Results[0].Text != "original" {
		t.Fatalf("lost source on failed replacement: %+v", r)
	}
}

func TestSQLiteConcurrentReplacementsDoNotLoseUpdates(t *testing.T) {
	started, release := make(chan struct{}, 2), make(chan struct{})
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	cfg.Extractor = extractorFunc(func(_ context.Context, r RetainRequest) ([]ExtractedFact, error) {
		started <- struct{}{}
		<-release
		return []ExtractedFact{{Text: r.Content, Type: "world"}}, nil
	})
	s := openTest(t, cfg)
	done := make(chan error, 2)
	for _, text := range []string{"first writer", "second writer"} {
		go func(text string) {
			_, err := s.Retain(context.Background(), testScope, RetainRequest{DocumentID: "source", Content: text})
			done <- err
		}(text)
	}
	<-started
	<-started
	close(release)
	a, b := <-done, <-done
	if !((a == nil && errors.Is(b, ErrConflict)) || (b == nil && errors.Is(a, ErrConflict))) {
		t.Fatalf("expected one winner and one conflict: %v %v", a, b)
	}
	if len(recallTest(t, s, testScope, "writer").Results) != 1 {
		t.Fatal("concurrent replacement produced inconsistent facts")
	}
}

func TestSQLiteSnapshotRestoresWithoutWAL(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, testConfig(filepath.Join(dir, "memory.db")))
	retainTest(t, s, testScope, "chat", "checkpointed Alice")
	before := recallTest(t, s, testScope, "Alice")
	snapshot := filepath.Join(dir, "snapshot.db")
	if err := s.Snapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(context.Background(), snapshot); err == nil {
		t.Fatal("overwrote existing checkpoint")
	}
	info, err := os.Stat(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot permissions: %v", info.Mode())
	}
	restored := openTest(t, testConfig(snapshot))
	after := recallTest(t, restored, testScope, "Alice")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("restore differs: before=%+v after=%+v", before, after)
	}
}

func TestSQLiteRetrievalArmsRerankAndBudget(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	cfg.Embedder = embedder{id: "test", vectors: map[string][]float32{"Alice": {1, 0}, "Alice joins Acme": {1, 0}, "Beta deadline": {0, 1}}}
	cfg.Extractor = extractorFunc(func(_ context.Context, r RetainRequest) ([]ExtractedFact, error) {
		return []ExtractedFact{
			{Text: "Alice joins Acme", Type: "world", Entities: []string{"Alice"}},
			{Text: "Beta deadline", Type: "experience", Entities: []string{"Different entity"}, OccurredStart: &start, OccurredEnd: &start, CausalRelations: []CausalLink{{TargetIndex: 0, RelationType: "caused_by"}}},
		}, nil
	})
	cfg.Reranker = rerankFunc(func(_ context.Context, _ string, docs []string) ([]float64, error) {
		scores := make([]float64, len(docs))
		for i, d := range docs {
			if strings.Contains(d, "Beta deadline") {
				scores[i] = 10
			}
		}
		return scores, nil
	})
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "chat", "input")
	r, err := s.Recall(context.Background(), testScope, RecallRequest{Query: "Alice", MaxTokens: 32768, TemporalWindow: &TemporalWindow{Start: start, End: start.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Results) != 2 || r.Reranked == nil || !*r.Reranked || r.Results[0].Text != "Beta deadline" {
		t.Fatalf("rerank/graph: %+v", r)
	}
	first := r.Results[0]
	if first.SourceRanks["graph"] != 0 || first.SourceRanks["temporal"] == 0 || first.SourceRanks["semantic"] != 0 {
		t.Fatalf("retrieval provenance: %+v", first)
	}
	r, err = s.Recall(context.Background(), testScope, RecallRequest{Query: "Alice", Types: []string{"world"}, MaxTokens: 32768})
	if err != nil || len(r.Results) != 1 || r.Results[0].Type != "world" {
		t.Fatalf("type filter: %+v %v", r, err)
	}
	r, err = s.Recall(context.Background(), testScope, RecallRequest{Query: "Alice", MaxTokens: 1})
	if err != nil || len(r.Results) != 0 {
		t.Fatalf("token budget: %+v %v", r, err)
	}
	// FTS operators in untrusted queries must become ordinary terms.
	if _, err = s.Recall(context.Background(), testScope, RecallRequest{Query: `Alice OR * NEAR("bad") - "`}); err != nil {
		t.Fatal(err)
	}
}

// This reproduces the first live failure against Hindsight: unstemmed "work"
// did not match "works", while graph seeds received an extra fusion vote.
func TestSQLiteEmploymentQueryStemsWordsWithoutDoubleCountingGraphSeeds(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	career := "Alice works at Acme and leads infrastructure"
	preference := "Alice prefers Go for backend services"
	query := "Where does Alice work?"
	cfg.Embedder = embedder{id: "test", vectors: map[string][]float32{query: {1, 0}, career: {1, 0}, preference: {0.8, 0.6}}}
	cfg.Extractor = extractorFunc(func(_ context.Context, r RetainRequest) ([]ExtractedFact, error) {
		return []ExtractedFact{{Text: r.Content, Type: "world", Entities: []string{"Alice"}}}, nil
	})
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "career", career)
	retainTest(t, s, testScope, "preferences", preference)
	r, err := s.Recall(context.Background(), testScope, RecallRequest{Query: query, MaxTokens: 32768})
	if err != nil || len(r.Results) != 2 || r.Results[0].DocumentID != "career" {
		t.Fatalf("employment recall: %+v %v", r, err)
	}
	for _, result := range r.Results {
		if result.SourceRanks["graph"] != 0 {
			t.Fatal("semantic seed was also counted as a graph discovery")
		}
	}
}

func TestSQLiteRejectsModelChangesAndInvalidEmbeddings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	s := openTest(t, testConfig(path))
	retainTest(t, s, testScope, "chat", "original")
	s.Close()
	cfg := testConfig(path)
	cfg.Embedder = embedder{id: "different-model"}
	changed := openTest(t, cfg)
	if _, err := changed.Recall(context.Background(), testScope, RecallRequest{Query: "original"}); err == nil {
		t.Fatal("mixed embedding models")
	}
	if _, err := changed.Retain(context.Background(), testScope, RetainRequest{DocumentID: "other", Content: "new"}); err == nil {
		t.Fatal("retained with incompatible model")
	}
	bad := testConfig(filepath.Join(t.TempDir(), "bad.db"))
	bad.Embedder = embedder{id: "test", vectors: map[string][]float32{"invalid": {float32(math.NaN()), 1}}}
	invalid := openTest(t, bad)
	if _, err := invalid.Retain(context.Background(), testScope, RetainRequest{DocumentID: "bad", Content: "invalid"}); err == nil {
		t.Fatal("accepted NaN")
	}
	var n int
	invalid.db.QueryRow(`SELECT count(*) FROM memory_documents`).Scan(&n)
	if n != 0 {
		t.Fatal("partial invalid document persisted")
	}
}

func TestFusionMatchesUpstreamRRF(t *testing.T) {
	facts := map[string]candidate{"a": {fact: Fact{ID: "a"}}, "b": {fact: Fact{ID: "b"}}}
	r := fuse(facts, [][]ranked{{{"a", 1}, {"b", 0.5}}, {{"b", 10}}, nil, nil}, 100)
	if len(r) != 2 || r[0].ID != "b" || math.Abs(r[0].Score-(1.0/62+1.0/61)) > 1e-12 {
		t.Fatalf("RRF changed: %+v", r)
	}
}

func TestTemporalInferenceAndUnicodeChunks(t *testing.T) {
	reference := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	w := inferWindow("what happened yesterday?", reference)
	if w == nil || w.Start.Format("2006-01-02") != "2026-10-01" {
		t.Fatalf("relative date: %+v", w)
	}
	w = inferWindow("from 2026-09-01 to 2026-09-30", reference)
	if w == nil || w.End.Day() != 30 {
		t.Fatalf("range: %+v", w)
	}
	text := strings.Repeat("日本語🙂", 4000)
	if got := strings.Join(splitChunks(text, 6000), ""); got != text {
		t.Fatal("chunking corrupted Unicode")
	}
}

func TestSQLiteKeywordSearchDropsEnglishStopWords(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	patient := "The patient consulted Dr Thompson"
	noise := "Who does the detective know in the room"
	query := "Who does the patient see?"
	cfg.Embedder = embedder{id: "test", vectors: map[string][]float32{patient: {0, 1}, noise: {0, 1}, query: {1, 0}, "the and who": {1, 0}}}
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "patient", patient)
	retainTest(t, s, testScope, "noise", noise)
	result := recallTest(t, s, testScope, query)
	if len(result.Results) != 1 || result.Results[0].DocumentID != "patient" || result.Results[0].SourceRanks["bm25"] == 0 {
		t.Fatalf("stop words admitted irrelevant evidence: %+v", result)
	}
	if result := recallTest(t, s, testScope, "the and who"); len(result.Results) != 0 {
		t.Fatal("stop-word-only query produced keyword evidence")
	}
}
