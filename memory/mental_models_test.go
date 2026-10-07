package memory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func mentalModelTestReflector(s *SQLite, scope Scope) ReflectionModel {
	return reflectionFunc(func(_ context.Context, m []ReflectionMessage, forced string) (ReflectionMessage, error) {
		if forced != "" {
			return reflectionCall(forced, forced, map[string]any{"reason": "search", "query": "Paris"}), nil
		}
		var result struct {
			Memories []Result `json:"memories"`
		}
		if err := json.Unmarshal([]byte(m[len(m)-1].Content), &result); err != nil {
			return ReflectionMessage{}, err
		}
		return reflectionCall("done", "done", map[string]any{"answer": "Alice lives in Paris.", "memory_ids": []string{result.Memories[0].ID}}), nil
	})
}
func TestMentalModelRefreshScopeFreshnessAndForget(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	ctx := context.Background()
	retainTest(t, s, testScope, "doc", "Alice lives in Paris.")
	if err := s.CreateMentalModel(ctx, testScope, "home", "Alice home", "Where does Alice live?"); err != nil {
		t.Fatal(err)
	}
	m, err := s.RefreshMentalModel(ctx, testScope, "home", mentalModelTestReflector(s, testScope))
	if err != nil || m.IsStale || len(m.SourceFactIDs) != 1 {
		t.Fatalf("%+v %v", m, err)
	}
	other := Scope{testScope.AppID, "other"}
	if _, err = s.ReadMentalModel(ctx, other, "home"); err == nil {
		t.Fatal("cross-bank read succeeded")
	}
	if models, err := s.SearchMentalModels(ctx, testScope, "Alice", 5); err != nil || len(models) != 1 {
		t.Fatalf("%+v %v", models, err)
	}
	// A known summary ID in the wrong reference array is normalized; an
	// unrelated ID is never accepted. Refresh still bypasses these summaries.
	hierarchical := reflectionFunc(func(_ context.Context, messages []ReflectionMessage, forced string) (ReflectionMessage, error) {
		if forced != "" {
			if forced != "search_mental_models" {
				t.Fatalf("wrong first retrieval: %s", forced)
			}
			return reflectionCall("summaries", forced, map[string]any{"reason": "curated topic first", "query": "Alice"}), nil
		}
		return reflectionCall("done", "done", map[string]any{"answer": "Alice lives in Paris.", "memory_ids": []string{"home"}}), nil
	})
	answer, err := s.Reflect(ctx, testScope, ReflectRequest{Query: "Where does Alice live?"}, hierarchical)
	if err != nil || len(answer.MentalModelIDs) != 1 || answer.MentalModelIDs[0] != "home" || len(answer.MemoryIDs) != 0 || len(answer.SourceFactIDs) != 1 {
		t.Fatalf("hierarchical citations: %+v %v", answer, err)
	}
	retainTest(t, s, testScope, "new", "Alice likes tea.")
	m, err = s.ReadMentalModel(ctx, testScope, "home")
	if err != nil || !m.IsStale {
		t.Fatalf("new evidence did not stale summary: %+v %v", m, err)
	}
	if err = s.Forget(ctx, testScope, "doc"); err != nil {
		t.Fatal(err)
	}
	m, err = s.ReadMentalModel(ctx, testScope, "home")
	if err != nil || m.Content != "" || len(m.SourceFactIDs) != 0 || !m.IsStale {
		t.Fatalf("forgotten source survives in generated text: %+v %v", m, err)
	}
}
func TestMentalModelRefreshRejectsConcurrentSourceMutation(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	ctx := context.Background()
	retainTest(t, s, testScope, "doc", "Paris")
	if err := s.CreateMentalModel(ctx, testScope, "home", "home", "home"); err != nil {
		t.Fatal(err)
	}
	base := mentalModelTestReflector(s, testScope)
	model := reflectionFunc(func(c context.Context, m []ReflectionMessage, f string) (ReflectionMessage, error) {
		turn, err := base.ReflectionStep(c, m, f)
		if f == "" {
			err = s.Forget(c, testScope, "doc")
		}
		return turn, err
	})
	if _, err := s.RefreshMentalModel(ctx, testScope, "home", model); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale refresh: %v", err)
	}
}

func TestMentalModelSnippetRequiresReadBeforeCitation(t *testing.T) {
	s := openTest(t, testConfig(filepath.Join(t.TempDir(), "memory.db")))
	ctx := context.Background()
	retainTest(t, s, testScope, "doc", "Alice lives in Paris.")
	for _, id := range []string{"one", "two"} {
		if err := s.CreateMentalModel(ctx, testScope, id, "Alice home", "Where does Alice live?"); err != nil {
			t.Fatal(err)
		}
		base := mentalModelTestReflector(s, testScope)
		long := reflectionFunc(func(c context.Context, m []ReflectionMessage, f string) (ReflectionMessage, error) {
			answer, err := base.ReflectionStep(c, m, f)
			if err == nil && answer.ToolCalls[0].Function.Name == "done" {
				var args map[string]any
				_ = json.Unmarshal([]byte(answer.ToolCalls[0].Function.Arguments), &args)
				args["answer"] = strings.Repeat("Alice lives in Paris. ", 30)
				return reflectionCall("done", "done", args), nil
			}
			return answer, err
		})
		if _, err := s.RefreshMentalModel(ctx, testScope, id, long); err != nil {
			t.Fatal(err)
		}
	}
	for _, read := range []bool{false, true} {
		step := 0
		id := ""
		model := reflectionFunc(func(_ context.Context, m []ReflectionMessage, forced string) (ReflectionMessage, error) {
			step++
			if step == 1 {
				return reflectionCall("search", "search_mental_models", map[string]any{"reason": "curated", "query": "Alice"}), nil
			}
			if step == 2 {
				var result struct {
					Models []MentalModel `json:"mental_models"`
				}
				_ = json.Unmarshal([]byte(m[len(m)-1].Content), &result)
				if len(result.Models) != 2 || len(result.Models[1].SourceFactIDs) != 0 || !strings.HasSuffix(result.Models[1].Content, "...") {
					t.Fatalf("expected snippet: %+v", result)
				}
				id = result.Models[1].ID
				if read {
					return reflectionCall("read", "read_mental_models", map[string]any{"reason": "relevant snippet", "mental_model_ids": []string{id}}), nil
				}
			}
			return reflectionCall("done", "done", map[string]any{"answer": "Alice lives in Paris.", "mental_model_ids": []string{id}}), nil
		})
		_, err := s.Reflect(ctx, testScope, ReflectRequest{Query: "Where does Alice live?"}, model)
		if read && err != nil {
			t.Fatal(err)
		}
		if !read && err == nil {
			t.Fatal("unread snippet accepted as evidence")
		}
	}
}
