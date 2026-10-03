package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeywordIndexIncludesContextEntitiesAndReadableDates(t *testing.T) {
	day := time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)
	f := Fact{Text: "They moved teams", Context: "customer interview", Entities: []string{"Alice", "Acme"}, OccurredStart: &day, OccurredEnd: &day}
	if got := KeywordText(f); got != "They moved teams customer interview Alice Acme June 10 2024" {
		t.Fatal(got)
	}
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	s.cfg.Extractor = extractorFunc(func(context.Context, RetainRequest) ([]ExtractedFact, error) {
		return []ExtractedFact{{Text: f.Text, Type: "world", Entities: f.Entities, OccurredStart: &day, OccurredEnd: &day}}, nil
	})
	_, err := s.Retain(context.Background(), testScope, RetainRequest{DocumentID: "doc", Content: "source", Context: f.Context, Timestamp: day})
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"customer", "Acme", "June"} {
		if result := recallTest(t, s, testScope, query); len(result.Results) == 0 || result.Results[0].SourceRanks["bm25"] == 0 {
			t.Errorf("missing keyword evidence for %s: %+v", query, result)
		}
	}
}

func TestVersionThreeKeywordMigrationPreservesFactsAndScopes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	s := openTest(t, testConfig(path))
	retainTest(t, s, testScope, "doc", "original facts")
	other := Scope{AppID: "other", BankID: testScope.BankID}
	retainTest(t, s, other, "doc", "other bank")
	if _, err := s.db.Exec("UPDATE memory_schema SET version=3"); err != nil {
		t.Fatal(err)
	}
	// Simulate the old index with a tokenizer different from the new prototype.
	table := ftsTable(testScope)
	if _, err := s.db.Exec("DROP TABLE " + table); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE VIRTUAL TABLE " + table + " USING fts5(text, tokenize='unicode61')"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	migrated := openTest(t, testConfig(path))
	result := recallTest(t, migrated, testScope, "original")
	if len(result.Results) != 1 || !strings.Contains(result.Results[0].Text, "original") || result.Results[0].SourceRanks["bm25"] != 1 {
		t.Fatalf("migration lost scoped index: %+v", result)
	}
	if result := recallTest(t, migrated, other, "other"); len(result.Results) != 1 || !strings.Contains(result.Results[0].Text, "other") {
		t.Fatal("migration mixed bank scopes")
	}
}
