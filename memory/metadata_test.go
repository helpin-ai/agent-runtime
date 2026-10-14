package memory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractedMetadataPreservesEvidenceAndHostOwnership(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	cfg.Extractor = extractorFunc(func(context.Context, RetainRequest) ([]ExtractedFact, error) {
		return []ExtractedFact{{Text: "A scoped operational lesson.", Type: "experience", Metadata: map[string]string{"source_ids": "[\"tool/1\"]", "bot_id": "untrusted"}}}, nil
	})
	s := openTest(t, cfg)
	_, err := s.Retain(context.Background(), testScope, RetainRequest{DocumentID: "episode/1", Content: "recorded task", Metadata: map[string]string{"bot_id": "trusted", "revision": "one"}})
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT payload FROM memory_facts WHERE document_id=?`, "episode/1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var fact Fact
	_ = json.Unmarshal([]byte(raw), &fact)
	if fact.Metadata["bot_id"] != "trusted" || fact.Metadata["source_ids"] != "[\"tool/1\"]" || fact.Metadata["revision"] != "one" {
		t.Fatal(fact.Metadata)
	}
}
func TestExtractedMetadataBounds(t *testing.T) {
	for _, metadata := range []map[string]string{{"source_ids": strings.Repeat("x", 8193)}, {strings.Repeat("x", 513): "value"}} {
		cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
		cfg.Extractor = extractorFunc(func(context.Context, RetainRequest) ([]ExtractedFact, error) {
			return []ExtractedFact{{Text: "A lesson", Type: "experience", Metadata: metadata}}, nil
		})
		s := openTest(t, cfg)
		if _, err := s.Retain(context.Background(), testScope, RetainRequest{DocumentID: "episode/1", Content: "evidence"}); err == nil {
			t.Fatal("oversized metadata accepted")
		}
	}
}

func TestInvalidationAllowsNewRevisionButNeverRevivesForgottenSource(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	retainTest(t, s, testScope, "episode/1", "Original evidence")
	if err := s.InvalidateDocument(context.Background(), testScope, "episode/1"); err != nil {
		t.Fatal(err)
	}
	if result := retainTest(t, s, testScope, "episode/1", "Changed evidence"); result.Unchanged {
		t.Fatal("invalidated source was not reprocessed")
	}
	if err := s.Forget(context.Background(), testScope, "episode/1"); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateDocument(context.Background(), testScope, "episode/1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retain(context.Background(), testScope, RetainRequest{DocumentID: "episode/1", Content: "Changed evidence"}); err != ErrForgotten {
		t.Fatal("invalidation revived a forgotten source", err)
	}
}
