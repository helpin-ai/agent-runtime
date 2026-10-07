package memory

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestChunkingMatchesPinnedUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/upstream-chunking.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		UpstreamCommit string `json:"upstream_commit"`
		Cases          []struct {
			Content  string   `json:"content"`
			Size     int      `json:"size"`
			Expected []string `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.UpstreamCommit != UpstreamCommit {
		t.Fatal("fixture upstream revision mismatch")
	}
	for index, test := range fixture.Cases {
		if got := splitChunks(test.Content, test.Size); !reflect.DeepEqual(got, test.Expected) {
			t.Errorf("case %d size %d: got %q, want %q", index, test.Size, got, test.Expected)
		}
	}
}

func TestRetrySplitMatchesPinnedUpstream(t *testing.T) {
	data, err := os.ReadFile("testdata/upstream-retry-split.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		UpstreamCommit string `json:"upstream_commit"`
		Cases          []struct {
			Content  string   `json:"content"`
			Expected []string `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.UpstreamCommit != UpstreamCommit {
		t.Fatal("upstream mismatch")
	}
	for i, test := range fixture.Cases {
		left, right, ok := splitOutputRetry(test.Content)
		var got []string
		if ok {
			got = []string{left, right}
		}
		if !reflect.DeepEqual(got, test.Expected) {
			t.Errorf("case %d: got %q want %q", i, got, test.Expected)
		}
	}
}
