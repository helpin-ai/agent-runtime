package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDerivedRetrievalMatchesPinnedFunctions(t *testing.T) {
	b, err := os.ReadFile("testdata/upstream-derived-retrieval.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		UpstreamCommit string    `json:"upstream_commit"`
		Now            time.Time `json:"now"`
		Scoring        []struct {
			Facts    []Fact `json:"facts"`
			Expected []struct {
				ID    string  `json:"id"`
				Score float64 `json:"score"`
			} `json:"expected"`
		} `json:"scoring"`
		Interleave struct {
			Arms     [][]string `json:"arms"`
			Expected []string   `json:"expected"`
		} `json:"interleave"`
		ObservationPrompt json.RawMessage `json:"observation_prompt"`
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.UpstreamCommit != UpstreamCommit {
		t.Fatal("upstream differs")
	}
	for _, test := range fixture.Scoring {
		results := make([]Result, len(test.Facts))
		for i, f := range test.Facts {
			results[i] = Result{Fact: f, Score: 1 / float64(61+i)}
		}
		combinedScoring(results, fixture.Now, nil, false)
		for i, r := range results {
			if r.ID != test.Expected[i].ID || math.Abs(r.Score-test.Expected[i].Score) > 1e-12 {
				t.Fatalf("%d candidates at %d: %+v expected %+v", len(results), i, r, test.Expected[i])
			}
		}
	}
	facts := map[string]candidate{}
	arms := make([][]ranked, len(fixture.Interleave.Arms))
	for a, ids := range fixture.Interleave.Arms {
		for _, id := range ids {
			facts[id] = candidate{fact: Fact{ID: id}}
			arms[a] = append(arms[a], ranked{id, 1})
		}
	}
	got := []string{}
	for _, r := range interleaveResults(facts, arms, 100) {
		got = append(got, r.ID)
	}
	if !reflect.DeepEqual(got, fixture.Interleave.Expected) {
		t.Fatalf("interleave %v expected %v", got, fixture.Interleave.Expected)
	}
	when := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	projected, err := consolidationObservationJSON([]Observation{{ID: "observation", Text: "Alice lives in Paris.", SourceFactIDs: []string{"a", "a", "b"}, MentionedAt: when, SourceMemories: []Fact{{Text: "Alice moved to Paris.", Context: strings.Repeat("界", 205), MentionedAt: when}}}})
	if err != nil {
		t.Fatal(err)
	}
	var actual, expected any
	if err = json.Unmarshal(projected, &actual); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(fixture.ObservationPrompt, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("prompt projection differs:\ngot %s\nwant %s", projected, fixture.ObservationPrompt)
	}
}

func TestMergedCandidateCapIsIndependentOfArmLimit(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	cfg.CandidateLimit = 1000
	cfg.Extractor = extractorFunc(func(context.Context, RetainRequest) ([]ExtractedFact, error) {
		facts := make([]ExtractedFact, 320)
		for i := range facts {
			facts[i] = ExtractedFact{Text: fmt.Sprintf("evidence %d", i), Type: "world"}
		}
		return facts, nil
	})
	cfg.Reranker = rerankFunc(func(_ context.Context, _ string, docs []string) ([]float64, error) {
		if len(docs) != 300 {
			t.Fatalf("reranker received %d candidates, upstream default cap is 300", len(docs))
		}
		scores := make([]float64, len(docs))
		for i := range scores {
			scores[i] = 0.5
		}
		return scores, nil
	})
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "doc", "evidence")
	recallTest(t, s, testScope, "evidence")
}

func TestReflectReservesFinalIterationForGroundedAnswer(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	retainTest(t, s, testScope, "doc", "Alice lives in Paris.")
	var evidence string
	model := reflectionFunc(func(_ context.Context, m []ReflectionMessage, forced string) (ReflectionMessage, error) {
		switch forced {
		case "search_observations":
			return reflectionCall("obs", forced, map[string]any{"query": "Paris"}), nil
		case "recall":
			return reflectionCall("raw", forced, map[string]any{"query": "Paris"}), nil
		case "done":
			var r struct {
				Memories []Result `json:"memories"`
			}
			if err := json.Unmarshal([]byte(m[len(m)-1].Content), &r); err != nil {
				return ReflectionMessage{}, err
			}
			evidence = r.Memories[0].ID
			return reflectionCall("answer", forced, map[string]any{"answer": "Paris", "memory_ids": []string{evidence}}), nil
		default:
			t.Fatal("last iteration allowed further retrieval")
			return ReflectionMessage{}, nil
		}
	})
	r, err := s.Reflect(context.Background(), testScope, ReflectRequest{Query: "Where does Alice live?", MaxSteps: 3}, model)
	if err != nil || r.Answer != "Paris" || len(r.SourceFactIDs) != 1 || r.SourceFactIDs[0] != evidence {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestObservationGraphTraversesSourceEntitiesAndIsScoped(t *testing.T) {
	cfg := testConfig(filepath.Join(t.TempDir(), "memory.db"))
	cfg.Extractor = extractorFunc(func(_ context.Context, r RetainRequest) ([]ExtractedFact, error) {
		return []ExtractedFact{{Text: r.Content, Type: "world", Entities: []string{"shared-team"}}}, nil
	})
	cfg.Embedder = embedder{id: "test", vectors: map[string][]float32{"Alpha": {1, 0}, "Beta": {0, 1}, "Summary Alpha": {1, 0}, "Summary Beta": {0, 1}}}
	s := openTest(t, cfg)
	retainTest(t, s, testScope, "alpha", "Alpha")
	retainTest(t, s, testScope, "beta", "Beta")
	model := consolidationFunc(func(_ context.Context, facts []Fact, _ []Observation) (ConsolidationPlan, error) {
		plan := ConsolidationPlan{}
		for _, f := range facts {
			plan.Creates = append(plan.Creates, ConsolidationAction{Text: "Summary " + f.Text, SourceFactIDs: []string{f.ID}})
		}
		return plan, nil
	})
	if _, err := s.Consolidate(context.Background(), testScope, model, 100); err != nil {
		t.Fatal(err)
	}
	other := Scope{AppID: "app", BankID: "other"}
	retainTest(t, s, other, "secret", "Alpha")
	if _, err := s.Consolidate(context.Background(), other, model, 100); err != nil {
		t.Fatal(err)
	}
	observations, err := s.SearchObservations(context.Background(), testScope, "Alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]bool{}
	for _, o := range observations {
		texts[o.Text] = true
	}
	if len(observations) != 2 || !texts["Summary Beta"] {
		t.Fatalf("source -> entity -> source -> observation traversal failed: %+v", observations)
	}
}
