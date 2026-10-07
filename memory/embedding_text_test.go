package memory

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestFactEmbeddingTextMatchesPinnedUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/upstream-embedding-text.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Fact        ExtractedFact `json:"fact"`
		MentionedAt *time.Time    `json:"mentioned_at"`
		Expected    string        `json:"expected"`
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		mentioned := time.Time{}
		if c.MentionedAt != nil {
			mentioned = *c.MentionedAt
		}
		if got := FactEmbeddingText(c.Fact, mentioned); got != c.Expected {
			t.Errorf("case %d: got %q want %q", i, got, c.Expected)
		}
	}
}
