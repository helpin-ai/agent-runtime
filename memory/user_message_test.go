package memory

import (
	"encoding/json"
	"os"
	"testing"
)

func TestExtractionUserMessageMatchesPinnedUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/upstream-user-message.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Request  RetainRequest `json:"request"`
		Chunk    string        `json:"chunk"`
		Index    int           `json:"index"`
		Total    int           `json:"total"`
		Expected string        `json:"expected"`
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		if got := extractionUserMessage(c.Request, c.Chunk, c.Index, c.Total); got != c.Expected {
			t.Errorf("case %d: got %q want %q", i, got, c.Expected)
		}
	}
}
