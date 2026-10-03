package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

type reflectionFunc func(context.Context, []ReflectionMessage, string) (ReflectionMessage, error)

func (f reflectionFunc) ReflectionStep(c context.Context, m []ReflectionMessage, forced string) (ReflectionMessage, error) {
	return f(c, m, forced)
}
func reflectionCall(id, name string, args any) ReflectionMessage {
	content, _ := json.Marshal(args)
	call := ReflectionToolCall{ID: id, Type: "function"}
	call.Function.Name = name
	call.Function.Arguments = string(content)
	return ReflectionMessage{Role: "assistant", ToolCalls: []ReflectionToolCall{call}}
}

func TestReflectRequiresFallbackAndChecksCitations(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	ctx := context.Background()
	retainTest(t, s, testScope, "doc", "Alice lives in Paris.")
	model := reflectionFunc(func(_ context.Context, m []ReflectionMessage, forced string) (ReflectionMessage, error) {
		switch forced {
		case "search_observations":
			return reflectionCall("obs", "search_observations", map[string]any{"reason": "summaries first", "query": "Paris"}), nil
		case "recall":
			return reflectionCall("raw", "recall", map[string]any{"reason": "source fallback", "query": "Paris"}), nil
		}
		var result struct {
			Memories []Result `json:"memories"`
		}
		if err := json.Unmarshal([]byte(m[len(m)-1].Content), &result); err != nil {
			return ReflectionMessage{}, err
		}
		return reflectionCall("final", "done", map[string]any{"answer": "Alice lives in Paris.", "memory_ids": []string{result.Memories[0].ID}}), nil
	})
	r, err := s.Reflect(ctx, testScope, ReflectRequest{Query: "Where does Alice live?"}, model)
	if err != nil || r.Answer != "Alice lives in Paris." || r.Steps != 3 || len(r.SourceFactIDs) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	bad := reflectionFunc(func(_ context.Context, m []ReflectionMessage, forced string) (ReflectionMessage, error) {
		if forced != "" {
			return reflectionCall(forced, forced, map[string]any{"reason": "search", "query": "Paris"}), nil
		}
		return reflectionCall("done", "done", map[string]any{"answer": "secret", "memory_ids": []string{"foreign"}}), nil
	})
	if _, err = s.Reflect(ctx, testScope, ReflectRequest{Query: "Paris"}, bad); err == nil {
		t.Fatal("accepted unretrieved citation")
	}
}

func TestReflectRejectsConcurrentForget(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	ctx := context.Background()
	retainTest(t, s, testScope, "doc", "Alice lives in Paris.")
	model := reflectionFunc(func(_ context.Context, m []ReflectionMessage, forced string) (ReflectionMessage, error) {
		if forced != "" {
			return reflectionCall(forced, forced, map[string]any{"reason": "search", "query": "Paris"}), nil
		}
		var r struct {
			Memories []Result `json:"memories"`
		}
		if err := json.Unmarshal([]byte(m[len(m)-1].Content), &r); err != nil {
			return ReflectionMessage{}, err
		}
		if err := s.Forget(ctx, testScope, "doc"); err != nil {
			return ReflectionMessage{}, err
		}
		return reflectionCall("done", "done", map[string]any{"answer": "Paris", "memory_ids": []string{r.Memories[0].ID}}), nil
	})
	if _, err := s.Reflect(ctx, testScope, ReflectRequest{Query: "Paris"}, model); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale reflection: %v", err)
	}
}

func TestReflectChecksRawEvidenceWhenObservationIsStale(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	ctx := context.Background()
	retainTest(t, s, testScope, "old", "Alice lives in Paris.")
	consolidator := consolidationFunc(func(_ context.Context, facts []Fact, _ []Observation) (ConsolidationPlan, error) {
		return ConsolidationPlan{Creates: []ConsolidationAction{{Text: "Alice lives in Paris.", SourceFactIDs: []string{facts[0].ID}}}}, nil
	})
	if _, err := s.Consolidate(ctx, testScope, consolidator, 10); err != nil {
		t.Fatal(err)
	}
	retainTest(t, s, testScope, "new", "Alice now lives in Stockholm.")
	model := reflectionFunc(func(_ context.Context, messages []ReflectionMessage, forced string) (ReflectionMessage, error) {
		if forced == "search_observations" {
			return reflectionCall("obs", forced, map[string]any{"reason": "summaries first", "query": "Alice"}), nil
		}
		if forced != "recall" {
			t.Fatalf("stale observation did not force raw recall: %s", forced)
		}
		var result struct {
			Observations []Observation `json:"observations"`
		}
		if err := json.Unmarshal([]byte(messages[len(messages)-1].Content), &result); err != nil {
			return ReflectionMessage{}, err
		}
		if len(result.Observations) != 1 || !result.Observations[0].IsStale {
			t.Fatalf("missing stale evidence: %+v", result)
		}
		return ReflectionMessage{}, errors.New("confirmed raw fallback")
	})
	if _, err := s.Reflect(ctx, testScope, ReflectRequest{Query: "Where does Alice live?"}, model); err == nil || err.Error() != "confirmed raw fallback" {
		t.Fatalf("%v", err)
	}
}

func TestReflectSynthesizesWhenContextFills(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	s.cfg.CountTokens = countTokens
	ctx := context.Background()
	retainTest(t, s, testScope, "doc", "Alice lives in Paris.")
	calls := 0
	source := ""
	model := reflectionFunc(func(_ context.Context, messages []ReflectionMessage, forced string) (ReflectionMessage, error) {
		calls++
		if !strings.Contains(messages[0].Content, "Answer in one sentence.") {
			t.Fatal("host directive missing")
		}
		if forced == "done" {
			if source == "" {
				t.Fatal("synthesis lost evidence")
			}
			return reflectionCall("final", "done", map[string]any{"answer": "Alice lives in Paris.", "memory_ids": []string{source}}), nil
		}
		if len(messages) > 2 {
			var result struct {
				Memories []Result `json:"memories"`
			}
			_ = json.Unmarshal([]byte(messages[len(messages)-1].Content), &result)
			if len(result.Memories) > 0 {
				source = result.Memories[0].ID
			}
		}
		name := forced
		if name == "" {
			name = "recall"
		}
		turn := reflectionCall(fmt.Sprint(calls), name, map[string]any{"reason": "verify", "query": "Paris"})
		turn.Content = strings.Repeat("reasoning ", 2500)
		return turn, nil
	})
	r, err := s.Reflect(ctx, testScope, ReflectRequest{Query: "Where does Alice live?", MaxContextTokens: 8192, MaxSteps: 20, Directives: []string{"Answer in one sentence."}}, model)
	if err != nil || r.Steps >= 20 || len(r.SourceFactIDs) != 1 {
		t.Fatalf("context synthesis: %+v %v", r, err)
	}
}
