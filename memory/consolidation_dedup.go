package memory

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

//go:embed upstream/consolidation-dedup.txt
var consolidationDedupPrompt string

//go:embed upstream/consolidation-dedup-schema.json
var consolidationDedupSchema []byte

type ObservationReconciliation struct {
	Action string `json:"action"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}
type ObservationReconciler interface {
	ReconcileObservations(context.Context, string, string) (ObservationReconciliation, error)
}

func (p *Provider) ReconcileObservations(ctx context.Context, newText, existing string) (ObservationReconciliation, error) {
	prompt := strings.NewReplacer("{new}", newText, "{existing}", existing, "{{", "{", "}}", "}").Replace(consolidationDedupPrompt)
	body := map[string]any{"model": p.cfg.ExtractionModel, "messages": []map[string]string{{"role": "user", "content": prompt}}, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "_DedupDecision", "schema": json.RawMessage(consolidationDedupSchema)}}}
	for k, v := range p.cfg.ExtractionOptions {
		body[k] = v
	}
	var response struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := jsonHTTP(ctx, p.client, http.MethodPost, p.cfg.BaseURL+"/chat/completions", p.cfg.APIKey, body, &response); err != nil {
		return ObservationReconciliation{}, err
	}
	// Upstream treats invalid structured verdicts as KEEP; a transport failure
	// fails the batch and leaves all input facts eligible for retry.
	keep := ObservationReconciliation{Action: "keep", Reason: "invalid structured response"}
	if len(response.Choices) != 1 || response.Choices[0].FinishReason != "stop" {
		return keep, nil
	}
	var decision ObservationReconciliation
	if err := json.Unmarshal([]byte(response.Choices[0].Message.Content), &decision); err != nil {
		return keep, nil
	}
	decision.Action = strings.ToLower(strings.TrimSpace(decision.Action))
	if decision.Action != "merge" && decision.Action != "keep" {
		return keep, nil
	}
	return decision, nil
}

// Prepare the upstream 0.97 cosine / top-five near-twin adjudication before
// entering the write transaction. The bank CAS protects targets and source proofs.
func (s *SQLite) deduplicateObservations(ctx context.Context, scope Scope, model Consolidator, prepared []Observation, vectors [][]float32, bankObservations []Observation) ([]Observation, [][]float32, []string, error) {
	reconciler, ok := model.(ObservationReconciler)
	if !ok {
		return prepared, vectors, nil, nil
	}
	old := map[string]Observation{}
	oldVectors := map[string][]float32{}
	for _, o := range bankObservations {
		old[o.ID] = o
		var value string
		if err := s.db.QueryRowContext(ctx, `SELECT embedding FROM memory_observations WHERE app_id=? AND bank_id=? AND id=?`, scope.AppID, scope.BankID, o.ID).Scan(&value); err != nil {
			return nil, nil, nil, err
		}
		var vector []float32
		if err := json.Unmarshal([]byte(value), &vector); err != nil {
			return nil, nil, nil, err
		}
		oldVectors[o.ID] = vector
	}
	deleted := []string{}
	targets := map[string]bool{}
	for i := range prepared {
		candidates := []ranked{}
		for id, v := range oldVectors {
			if len(v) != len(vectors[i]) {
				return nil, nil, nil, fmt.Errorf("stored observation dimensions differ")
			}
			candidates = append(candidates, ranked{id, cosine(v, vectors[i])})
		}
		candidates = top(candidates, 5)
		target := ""
		best := 0.97
		for _, r := range candidates {
			if r.id != prepared[i].ID && r.score >= best {
				target = r.id
				best = r.score
			}
		}
		if target == "" {
			continue
		}
		decision, err := reconciler.ReconcileObservations(ctx, prepared[i].Text, old[target].Text)
		if err != nil {
			return nil, nil, nil, err
		}
		if decision.Action != "merge" {
			continue
		}
		if targets[target] {
			return nil, nil, nil, ErrConflict
		}
		targets[target] = true
		previous := prepared[i].ID
		prepared[i].ID = target
		prepared[i].Text = strings.TrimSpace(decision.Text)
		if prepared[i].Text == "" {
			prepared[i].Text = old[target].Text
		}
		if len(prepared[i].Text) > 32768 {
			return nil, nil, nil, fmt.Errorf("merged observation text exceeds limit")
		}
		proofs := map[string]bool{}
		for _, id := range append(prepared[i].SourceFactIDs, old[target].SourceFactIDs...) {
			proofs[id] = true
		}
		prepared[i].SourceFactIDs = nil
		for id := range proofs {
			prepared[i].SourceFactIDs = append(prepared[i].SourceFactIDs, id)
		}
		sort.Strings(prepared[i].SourceFactIDs)
		if _, exists := old[previous]; exists {
			deleted = append(deleted, previous)
		}
		embedded, err := s.cfg.Embedder.Embed(ctx, []string{prepared[i].Text})
		if err != nil {
			return nil, nil, nil, err
		}
		if err = validateVectors(embedded, 1); err != nil {
			return nil, nil, nil, err
		}
		vectors[i] = embedded[0]
	}
	// Two prepared writes cannot silently overwrite one target's sources/text.
	seen := map[string]bool{}
	for _, o := range prepared {
		if seen[o.ID] {
			return nil, nil, nil, ErrConflict
		}
		seen[o.ID] = true
	}
	return prepared, vectors, deleted, nil
}
