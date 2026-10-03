package memory

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

func TestRecencyMatchesPinnedUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/upstream-recency.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		UpstreamCommit string    `json:"upstream_commit"`
		Now            time.Time `json:"now"`
		Cases          []struct {
			Fact     Fact    `json:"fact"`
			Expected float64 `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.UpstreamCommit != UpstreamCommit {
		t.Fatal("upstream mismatch")
	}
	for i, test := range fixture.Cases {
		if got := recency(test.Fact, fixture.Now); math.Abs(got-test.Expected) > 1e-12 {
			t.Errorf("case %d: got %v want %v", i, got, test.Expected)
		}
	}
}
func TestCombinedScoringPreservesCalibrationAndNormalizesLogits(t *testing.T) {
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct{ score, want float64 }{{0.007, 0.007}, {10, 1 / (1 + math.Exp(-10))}} {
		results := []Result{{Fact: Fact{ID: "fact"}, Score: test.score}}
		combinedScoring(results, now, nil, true)
		if math.Abs(results[0].Score-test.want) > 1e-12 {
			t.Fatalf("confidence changed: got=%v want=%v", results[0].Score, test.want)
		}
	}
	results := make([]Result, 100)
	for i := range results {
		results[i] = Result{Fact: Fact{ID: "old", MentionedAt: now.AddDate(-10, 0, 0)}, Score: 1 - float64(i)*0.001}
	}
	results[1].ID = "new"
	results[1].MentionedAt = now
	combinedScoring(results, now, nil, false)
	if results[0].ID != "new" {
		t.Fatal("recency did not adjust adjacent RRF ranks")
	}
}
