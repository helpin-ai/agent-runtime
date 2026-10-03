package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// SearchObservations returns derived evidence with its complete proof IDs. This
// search fuses semantic, keyword, proof-link and temporal evidence, then applies
// the same combined scoring as raw recall plus the upstream proof-count boost.
func (s *SQLite) SearchObservations(ctx context.Context, scope Scope, query string, limit int) ([]Observation, error) {
	return s.searchObservations(ctx, scope, query, limit, false)
}

func (s *SQLite) searchObservations(ctx context.Context, scope Scope, query string, limit int, interleave bool) ([]Observation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if _, err := (RecallRequest{Query: query, Limit: limit}).normalized(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("observation limit must be 1–100")
	}
	vectors, err := s.cfg.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if err = validateVectors(vectors, 1); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var model string
	var dims int
	err = tx.QueryRowContext(ctx, `SELECT model_id,dimensions FROM memory_banks WHERE app_id=? AND bank_id=?`, scope.AppID, scope.BankID).Scan(&model, &dims)
	if err == sql.ErrNoRows {
		return []Observation{}, nil
	}
	if err != nil {
		return nil, err
	}
	if model != s.cfg.Embedder.ModelID() || dims != len(vectors[0]) {
		return nil, fmt.Errorf("embedding model/dimensions differ from bank")
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_facts f WHERE app_id=? AND bank_id=? AND NOT EXISTS(SELECT 1 FROM memory_consolidated c WHERE c.fact_id=f.id)`, scope.AppID, scope.BankID).Scan(&pending); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload,embedding FROM memory_observations WHERE app_id=? AND bank_id=?`, scope.AppID, scope.BankID)
	if err != nil {
		return nil, err
	}
	observations := map[string]Observation{}
	facts := map[string]candidate{}
	semantic := []ranked{}
	for rows.Next() {
		var payload, embedding string
		if err = rows.Scan(&payload, &embedding); err != nil {
			break
		}
		var o Observation
		var v []float32
		if err = json.Unmarshal([]byte(payload), &o); err != nil {
			break
		}
		o.IsStale = pending > 0
		if err = json.Unmarshal([]byte(embedding), &v); err != nil {
			break
		}
		if err = validateVectors([][]float32{v}, 1); err != nil {
			break
		}
		if len(v) != dims {
			err = fmt.Errorf("stored observation dimensions differ")
			break
		}
		score := cosine(vectors[0], v)
		observations[o.ID] = o
		facts[o.ID] = candidate{fact: Fact{ID: o.ID, Text: o.Text, Type: "observation", MentionedAt: o.MentionedAt, OccurredStart: o.OccurredStart, OccurredEnd: o.OccurredEnd}, vector: v, semantic: score}
		if score >= 0.3 {
			semantic = append(semantic, ranked{o.ID, score})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	semantic = top(semantic, s.cfg.CandidateLimit)
	keyword, err := observationKeywords(ctx, tx, query, facts, s.cfg.CandidateLimit)
	if err != nil {
		return nil, err
	}
	// Upstream observations traverse seed proofs -> entities -> other raw
	// facts -> their observations. Shared seed proofs alone are not an edge.
	seeds := map[string]bool{}
	seedSources := map[string]bool{}
	for _, r := range semantic[:min(20, len(semantic))] {
		seeds[r.id] = true
		for _, id := range observations[r.id].SourceFactIDs {
			seedSources[id] = true
		}
	}
	rawFacts := map[string]Fact{}
	rawRows, e := tx.QueryContext(ctx, `SELECT payload FROM memory_facts WHERE app_id=? AND bank_id=?`, scope.AppID, scope.BankID)
	if e != nil {
		return nil, e
	}
	for rawRows.Next() {
		var p string
		if e = rawRows.Scan(&p); e != nil {
			break
		}
		var f Fact
		if e = json.Unmarshal([]byte(p), &f); e != nil {
			break
		}
		rawFacts[f.ID] = f
	}
	if e == nil {
		e = rawRows.Err()
	}
	rawRows.Close()
	if e != nil {
		return nil, e
	}
	seedEntities := map[string]bool{}
	for id := range seedSources {
		for _, entity := range rawFacts[id].Entities {
			seedEntities[normalizeEntity(entity)] = true
		}
	}
	byEntity := map[string][]string{}
	for id, f := range rawFacts {
		seen := map[string]bool{}
		for _, entity := range f.Entities {
			entity = normalizeEntity(entity)
			if seedEntities[entity] && !seen[entity] {
				byEntity[entity] = append(byEntity[entity], id)
				seen[entity] = true
			}
		}
	}
	connected := map[string]bool{}
	for _, ids := range byEntity {
		sort.Sort(sort.Reverse(sort.StringSlice(ids)))
		for _, id := range ids[:min(200, len(ids))] {
			if !seedSources[id] {
				connected[id] = true
			}
		}
	}
	graphScores := map[string]float64{}
	for id, o := range observations {
		if seeds[id] {
			continue
		}
		shared := map[string]bool{}
		for _, proof := range o.SourceFactIDs {
			if connected[proof] {
				shared[proof] = true
			}
		}
		if len(shared) > 0 {
			graphScores[id] = math.Tanh(float64(len(shared)) * 0.5)
		}
	}
	window := inferWindow(query, time.Now().UTC())
	arms := [][]ranked{semantic, keyword, topMap(graphScores, s.cfg.CandidateLimit), temporalCandidates(facts, window, s.cfg.CandidateLimit)}
	results := fuse(facts, arms, s.cfg.RerankerMaxCandidates)
	if interleave {
		results = interleaveResults(facts, arms, s.cfg.RerankerMaxCandidates)
	} else {
		neural := false
		if s.cfg.Reranker != nil && len(results) > 0 {
			texts := make([]string, len(results))
			for i, r := range results {
				texts[i] = r.Text
			}
			scores, e := s.cfg.Reranker.Rerank(ctx, query, texts)
			if e != nil {
				return nil, e
			}
			if len(scores) != len(results) {
				return nil, fmt.Errorf("reranker score count mismatch")
			}
			for i, v := range scores {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					return nil, fmt.Errorf("non-finite reranker score")
				}
				results[i].Score = v
			}
			sortResults(results)
			neural = true
		}
		combinedScoring(results, time.Now().UTC(), window, neural)
		for i := range results {
			proofNorm := 0.5
			if count := observations[results[i].ID].ProofCount; count >= 1 {
				proofNorm = math.Min(1, 0.5+math.Log(float64(count))/10)
			}
			results[i].Score *= 1 + 0.1*(proofNorm-0.5)
		}
		sortResults(results)
	}
	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]Observation, len(results))
	for i, r := range results {
		out[i] = observations[r.ID]
	}
	return out, tx.Commit()
}

// The temporary FTS index contains only this scoped snapshot and is removed
// before committing; no unrelated bank contributes term frequencies or rows.
func observationKeywords(ctx context.Context, tx *sql.Tx, query string, facts map[string]candidate, limit int) ([]ranked, error) {
	const name = "memory_observation_query_fts"
	if _, err := tx.ExecContext(ctx, `CREATE VIRTUAL TABLE temp.`+name+` USING fts5(id UNINDEXED,text,tokenize='porter unicode61')`); err != nil {
		return nil, err
	}
	defer tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+name)
	for id, c := range facts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+name+`(id,text) VALUES(?,?)`, id, KeywordText(c.fact)); err != nil {
			return nil, err
		}
	}
	terms := []string{}
	for _, term := range queryTerms(query) {
		if !englishStops[term] {
			terms = append(terms, "\""+term+"\"")
		}
	}
	if len(terms) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,-bm25(`+name+`) FROM `+name+` WHERE `+name+` MATCH ? ORDER BY bm25(`+name+`),id LIMIT ?`, strings.Join(terms, " OR "), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ranked{}
	for rows.Next() {
		var r ranked
		if err = rows.Scan(&r.id, &r.score); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
