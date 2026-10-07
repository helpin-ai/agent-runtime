package memory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSQLiteSourceChunksDeduplicateScopeRestoreAndForget(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	cfg.Extractor = extractorFunc(func(_ context.Context, req RetainRequest) ([]ExtractedFact, error) {
		chunk := &ExtractedChunk{Index: 0, Text: req.Content}
		return []ExtractedFact{{Text: "first fact", Type: "world", SourceChunk: chunk}, {Text: "second fact", Type: "world", SourceChunk: chunk}}, nil
	})
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "doc", "Original source includes study with 24 subjects.")
	other := Scope{AppID: "other", BankID: testScope.BankID}
	retainTest(t, s, other, "doc", "private other bank")
	req := RecallRequest{Query: "study", IncludeChunks: true, MaxTokens: 32768, QueryTimestamp: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	result, err := s.Recall(context.Background(), testScope, req)
	if err != nil || len(result.Results) != 2 || len(result.Chunks) != 1 {
		t.Fatalf("chunks: %+v %v", result, err)
	}
	for _, chunk := range result.Chunks {
		if chunk.DocumentID != "doc" || chunk.Text != "Original source includes study with 24 subjects." {
			t.Fatalf("wrong source: %+v", chunk)
		}
	}
	for _, fact := range result.Results {
		if _, ok := result.Chunks[fact.ChunkID]; !ok {
			t.Fatal("missing source for fact")
		}
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.Snapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	cfg.Path = path
	restored := openTest(t, cfg)
	after, err := restored.Recall(context.Background(), testScope, req)
	if err != nil || !reflect.DeepEqual(result, after) {
		t.Fatalf("source snapshot differs: %+v %v", after, err)
	}
	if err := s.Forget(context.Background(), testScope, "doc"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_chunks WHERE app_id=?`, testScope.AppID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("source survived forget: %d %v", count, err)
	}
	if _, err := s.Recall(context.Background(), other, req); err != nil {
		t.Fatal(err)
	}
}
func TestSourceChunkBudgetTruncatesWithoutBreakingUnicode(t *testing.T) {
	output := RecallResult{Results: []Result{{Fact: Fact{ChunkID: "chunk"}}}}
	available := map[string]SourceChunk{"chunk": {ID: "chunk", DocumentID: "doc", Text: strings.Repeat("日本語🙂", 1000)}}
	if err := includeChunks(&output, available, 512, func(s string) int { return len(s) }); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(output.Chunks)
	chunk := output.Chunks["chunk"]
	if len(data) > 512 || !chunk.Truncated || !utf8.ValidString(chunk.Text) || chunk.Text == "" {
		t.Fatalf("invalid chunk budget: size=%d chunk=%+v", len(data), chunk)
	}
}
func TestSQLiteMigratesLegacySourceProvenance(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "doc", "original source")
	if _, err := s.db.Exec(`UPDATE memory_facts SET payload=json_remove(payload,'$.chunk_id')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM memory_chunks`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE memory_schema SET version=2`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	migrated := openTest(t, cfg)
	result, err := migrated.Recall(context.Background(), testScope, RecallRequest{Query: "source", IncludeChunks: true})
	if err != nil || len(result.Chunks) != 1 {
		t.Fatalf("migration lost source: %+v %v", result, err)
	}
	for _, chunk := range result.Chunks {
		if chunk.Text != "original source" {
			t.Fatal("source changed on upgrade")
		}
	}
}
