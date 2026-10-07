package memory

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestExtractionDatesMatchPinnedUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/upstream-extraction-dates.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input    *string `json:"input"`
		Expected *string `json:"expected"`
	}
	if err = json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got := parseExtractionDate(c.Input)
		if c.Expected == nil {
			if got != nil {
				t.Errorf("%v: expected unknown, got %s", c.Input, got)
			}
			continue
		}
		want, err := time.Parse(time.RFC3339Nano, *c.Expected)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || !got.Equal(want) {
			t.Errorf("%q: got %v want %s", *c.Input, got, want)
		}
	}
}

func TestInvalidModelDatesPreserveFactsAndStayUnknown(t *testing.T) {
	facts, err := parseExtractedFacts(`{"facts":[{"what":"Alice visited yesterday","fact_kind":"event","fact_type":"world","occurred_start":"N/A","occurred_end":"Unknown"}]}`, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || len(facts) != 1 || facts[0].OccurredStart != nil || facts[0].OccurredEnd != nil {
		t.Fatalf("invalid date discarded fact or invented date: %+v %v", facts, err)
	}
}
