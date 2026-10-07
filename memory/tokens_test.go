package memory

import (
	"encoding/json"
	"os"
	"testing"
)

func TestTokenCountsMatchPinnedHindsight(t *testing.T) {
	data, err := os.ReadFile("upstream/token-count-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Text   string
		Tokens int
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		if got := countTokens(f.Text); got != f.Tokens {
			t.Errorf("%q: got %d, reference %d", f.Text, got, f.Tokens)
		}
	}
}
