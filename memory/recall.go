package memory

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

type candidate struct {
	fact     Fact
	vector   []float32
	semantic float64
}
type ranked struct {
	id    string
	score float64
}

func (s *SQLite) Recall(ctx context.Context, scope Scope, req RecallRequest) (RecallResult, error) {
	notReranked := false
	empty := RecallResult{Results: []Result{}, Reranked: &notReranked}
	if err := scope.Validate(); err != nil {
		return empty, err
	}
	req, err := req.normalized()
	if err != nil {
		return empty, err
	}
	vectors, err := s.cfg.Embedder.Embed(ctx, []string{req.Query})
	if err != nil {
		return empty, fmt.Errorf("embed query: %w", err)
	}
	if err = validateVectors(vectors, 1); err != nil {
		return empty, err
	}
	// All storage reads share a transaction; a replacement cannot mix old facts
	// with new keyword results. Model calls happen outside the transaction.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	var modelID string
	var dimensions int
	err = tx.QueryRowContext(ctx, `SELECT model_id,dimensions FROM memory_banks WHERE app_id=? AND bank_id=?`, scope.AppID, scope.BankID).Scan(&modelID, &dimensions)
	if err == sql.ErrNoRows {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	if modelID != s.cfg.Embedder.ModelID() || dimensions != len(vectors[0]) {
		return empty, fmt.Errorf("embedding model/dimensions differ from bank; reindex into a new bank")
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload,embedding FROM memory_facts WHERE app_id=? AND bank_id=? ORDER BY id`, scope.AppID, scope.BankID)
	if err != nil {
		return empty, err
	}
	facts := map[string]candidate{}
	semantic := []ranked{}
	for rows.Next() {
		var payload, embedding string
		if err = rows.Scan(&payload, &embedding); err != nil {
			rows.Close()
			return empty, err
		}
		var c candidate
		if err = json.Unmarshal([]byte(payload), &c.fact); err != nil {
			rows.Close()
			return empty, err
		}
		if !contains(req.Types, c.fact.Type) {
			continue
		}
		if err = json.Unmarshal([]byte(embedding), &c.vector); err != nil {
			rows.Close()
			return empty, err
		}
		if len(c.vector) != dimensions {
			rows.Close()
			return empty, fmt.Errorf("stored embedding dimensions mismatch")
		}
		c.semantic = cosine(vectors[0], c.vector)
		facts[c.fact.ID] = c
		if c.semantic >= 0.3 {
			semantic = append(semantic, ranked{c.fact.ID, c.semantic})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return empty, err
	}
	semantic = top(semantic, s.cfg.CandidateLimit)
	keyword, err := keywordCandidates(ctx, tx, scope, req.Query, facts, s.cfg.CandidateLimit)
	if err != nil {
		return empty, err
	}
	graph, err := graphCandidates(ctx, tx, scope, req.Query, facts, semantic, keyword, s.cfg.CandidateLimit)
	if err != nil {
		return empty, err
	}
	window := req.TemporalWindow
	if window == nil {
		window = inferWindow(req.Query, req.QueryTimestamp)
	}
	temporal := temporalCandidates(facts, window, s.cfg.CandidateLimit)
	chunks := map[string]SourceChunk{}
	if req.IncludeChunks {
		chunks, err = loadChunks(ctx, tx, scope)
		if err != nil {
			return empty, err
		}
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	results := fuse(facts, [][]ranked{semantic, keyword, graph, temporal}, s.cfg.RerankerMaxCandidates)
	reranked := false
	if s.cfg.Reranker != nil && len(results) > 0 {
		texts := make([]string, len(results))
		for i, r := range results {
			texts[i] = r.Text
			if r.Context != "" {
				texts[i] = r.Context + ": " + texts[i]
			}
			if r.OccurredStart != nil {
				texts[i] = "[Date: " + r.OccurredStart.Format("January 02, 2006 (2006-01-02)") + "] " + texts[i]
			}
		}
		scores, e := s.cfg.Reranker.Rerank(ctx, req.Query, texts)
		if e != nil {
			return empty, fmt.Errorf("rerank memory: %w", e)
		}
		if len(scores) != len(results) {
			return empty, fmt.Errorf("reranker score count mismatch")
		}
		for i, score := range scores {
			if math.IsNaN(score) || math.IsInf(score, 0) {
				return empty, fmt.Errorf("non-finite reranker score")
			}
			results[i].Score = score
		}
		// Preserve fusion order for tied reranker scores.
		sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
		reranked = true
	}
	now := req.QueryTimestamp
	if now.IsZero() {
		now = time.Now().UTC()
	}
	combinedScoring(results, now, window, reranked)
	output, err := budgetResults(results, req, s.cfg.CountTokens, reranked)
	if err != nil {
		return empty, err
	}
	if req.IncludeChunks {
		if err := includeChunks(&output, chunks, req.MaxChunkTokens, s.cfg.CountTokens); err != nil {
			return empty, err
		}
	}
	return output, nil
}

func contains(items []string, s string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}

func cosine(a, b []float32) float64 {
	var dot, aa, bb float64
	for i, x := range a {
		y := float64(b[i])
		xx := float64(x)
		dot += xx * y
		aa += xx * xx
		bb += y * y
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / math.Sqrt(aa*bb)
}

func top(items []ranked, limit int) []ranked {
	sort.Slice(items, func(i, j int) bool {
		if items[i].score == items[j].score {
			return items[i].id < items[j].id
		}
		return items[i].score > items[j].score
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func queryTerms(query string) []string {
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	seen := map[string]bool{}
	terms := []string{}
	for _, word := range words {
		if !seen[word] {
			terms = append(terms, word)
			seen[word] = true
		}
		if len(terms) == 64 {
			break
		}
	}
	return terms
}

//go:embed upstream/postgres-english.stop
var englishStopWords string

var englishStops = func() map[string]bool {
	words := map[string]bool{}
	for _, word := range strings.Fields(englishStopWords) {
		words[word] = true
	}
	return words
}()

func keywordCandidates(ctx context.Context, tx *sql.Tx, scope Scope, query string, facts map[string]candidate, limit int) ([]ranked, error) {
	terms := []string{}
	for _, term := range queryTerms(query) {
		if !englishStops[term] {
			terms = append(terms, term)
		}
	}
	if len(terms) == 0 {
		return nil, nil
	}
	for i := range terms {
		terms[i] = "\"" + terms[i] + "\""
	}
	// MATCH text is constructed from quoted alphanumeric tokens, not FTS syntax
	// supplied by the model. Scope is applied before LIMIT.
	fts := ftsTable(scope)
	rows, err := tx.QueryContext(ctx, `SELECT f.id,bm25(`+fts+`) FROM `+fts+` JOIN memory_facts f ON f.row_id=`+fts+`.rowid
 WHERE `+fts+` MATCH ? AND f.app_id=? AND f.bank_id=? ORDER BY bm25(`+fts+`),f.id`, strings.Join(terms, " OR "), scope.AppID, scope.BankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ranked{}
	for rows.Next() {
		var id string
		var score float64
		if err = rows.Scan(&id, &score); err != nil {
			return nil, err
		}
		if _, ok := facts[id]; ok {
			items = append(items, ranked{id, -score})
			if len(items) == limit {
				break
			}
		}
	}
	return items, rows.Err()
}

func graphCandidates(ctx context.Context, tx *sql.Tx, scope Scope, query string, facts map[string]candidate, semantic, keyword []ranked, limit int) ([]ranked, error) {
	byEntity := map[string][]string{}
	for id, c := range facts {
		for _, entity := range c.fact.Entities {
			name := normalizeEntity(entity)
			if name != "" {
				byEntity[name] = append(byEntity[name], id)
			}
		}
	}
	type edge struct {
		target, kind string
		weight       float64
	}
	adjacency := map[string][]edge{}
	rows, err := tx.QueryContext(ctx, `SELECT l.source_id,l.target_id,l.kind,l.weight FROM memory_links l JOIN memory_facts f ON f.id=l.source_id WHERE f.app_id=? AND f.bank_id=?`, scope.AppID, scope.BankID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var source, target, kind string
		var weight float64
		if err = rows.Scan(&source, &target, &kind, &weight); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := facts[source]; !ok {
			continue
		}
		if _, ok := facts[target]; !ok {
			continue
		}
		adjacency[source] = append(adjacency[source], edge{target, kind, weight})
		if kind == "semantic" {
			adjacency[target] = append(adjacency[target], edge{source, kind, weight})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	// Upstream link expansion takes at most 20 semantic seeds per fact type
	// above 0.3 similarity. Seeds are entry points, never graph discoveries.
	seeds := map[string]bool{}
	counts := map[string]int{}
	for _, item := range semantic {
		typeName := facts[item.id].fact.Type
		if counts[typeName] < 20 && item.score >= 0.3 {
			seeds[item.id] = true
			counts[typeName]++
		}
	}
	seedEntities := map[string]map[string]bool{}
	for id := range seeds {
		kind := facts[id].fact.Type
		if seedEntities[kind] == nil {
			seedEntities[kind] = map[string]bool{}
		}
		for _, entity := range facts[id].fact.Entities {
			seedEntities[kind][normalizeEntity(entity)] = true
		}
	}
	activation := map[string]float64{}
	for kind, entities := range seedEntities {
		shared := map[string]int{}
		for entity := range entities {
			ids := append([]string(nil), byEntity[entity]...)
			sort.Sort(sort.Reverse(sort.StringSlice(ids)))
			// The upstream per-entity window is capped before seed/type
			// exclusion, so excluded rows still consume their window slots.
			for _, id := range ids[:min(200, len(ids))] {
				if seeds[id] || facts[id].fact.Type != kind {
					continue
				}
				shared[id]++
			}
		}
		for id, count := range shared {
			activation[id] = math.Tanh(float64(count) * 0.5)
		}
	}
	semanticScores, causalScores := map[string]float64{}, map[string]float64{}
	for id := range seeds {
		for _, edge := range adjacency[id] {
			if facts[id].fact.Type != facts[edge.target].fact.Type {
				continue
			}
			if edge.kind == "semantic" {
				if !seeds[edge.target] {
					semanticScores[edge.target] = math.Max(semanticScores[edge.target], edge.weight)
				}
			} else {
				causalScores[edge.target] = math.Max(causalScores[edge.target], edge.weight)
			}
		}
	}
	for id, score := range semanticScores {
		activation[id] += score
	}
	for id, score := range causalScores {
		activation[id] += score
	}

	return topMap(activation, limit), nil
}

func topMap(scores map[string]float64, limit int) []ranked {
	items := make([]ranked, 0, len(scores))
	for id, score := range scores {
		items = append(items, ranked{id, score})
	}
	return top(items, limit)
}

func sortResults(results []Result) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
}

// Consolidation uses upstream's round-robin fusion: a semantic nearest twin
// must survive even when it has little lexical overlap or no graph links.
func interleaveResults(facts map[string]candidate, arms [][]ranked, limit int) []Result {
	seen := map[string]bool{}
	results := []Result{}
	for rank := 0; len(results) < limit; rank++ {
		found := false
		for _, arm := range arms {
			if rank >= len(arm) {
				continue
			}
			found = true
			id := arm[rank].id
			if seen[id] {
				continue
			}
			seen[id] = true
			results = append(results, Result{Fact: facts[id].fact, Score: float64(limit - len(results))})
			if len(results) == limit {
				break
			}
		}
		if !found {
			break
		}
	}
	return results
}

func temporalCandidates(facts map[string]candidate, w *TemporalWindow, limit int) []ranked {
	if w == nil {
		return nil
	}
	items := []ranked{}
	for id, c := range facts {
		start, end := c.fact.MentionedAt, c.fact.MentionedAt
		if c.fact.OccurredStart != nil {
			start = *c.fact.OccurredStart
			end = start
		}
		if c.fact.OccurredEnd != nil {
			end = *c.fact.OccurredEnd
		}
		if !start.After(w.End) && !end.Before(w.Start) {
			items = append(items, ranked{id, 1 + c.semantic})
		}
	}
	return top(items, limit)
}

var isoDate = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)

// Only unambiguous ISO dates and a small relative-date vocabulary are inferred.
// Hosts should provide TemporalWindow for richer date expressions.
func inferWindow(query string, reference time.Time) *TemporalWindow {
	dates := isoDate.FindAllString(query, 2)
	if len(dates) > 0 {
		start, err := time.Parse("2006-01-02", dates[0])
		if err != nil {
			return nil
		}
		end := start
		if len(dates) == 2 {
			end, err = time.Parse("2006-01-02", dates[1])
			if err != nil || end.Before(start) {
				return nil
			}
		}
		return &TemporalWindow{start, end.AddDate(0, 0, 1).Add(-time.Nanosecond)}
	}
	if reference.IsZero() {
		reference = time.Now().UTC()
	}
	day := time.Date(reference.Year(), reference.Month(), reference.Day(), 0, 0, 0, 0, reference.Location())
	for _, term := range queryTerms(query) {
		offset := 0
		switch term {
		case "today":
		case "yesterday":
			offset = -1
		case "tomorrow":
			offset = 1
		default:
			continue
		}
		start := day.AddDate(0, 0, offset)
		return &TemporalWindow{start, start.AddDate(0, 0, 1).Add(-time.Nanosecond)}
	}
	return relativeWindow(query, day)
}

// fuse ports Hindsight's reciprocal rank fusion (k=60), keeping per-arm ranks.
func fuse(facts map[string]candidate, arms [][]ranked, limit int) []Result {
	names := []string{"semantic", "bm25", "graph", "temporal"}
	scores := map[string]float64{}
	ranks := map[string]map[string]int{}
	for a, arm := range arms {
		for i, item := range arm {
			if ranks[item.id] == nil {
				ranks[item.id] = map[string]int{}
			}
			scores[item.id] += 1 / float64(60+i+1)
			ranks[item.id][names[a]] = i + 1
		}
	}
	ordered := topMap(scores, limit)
	results := make([]Result, 0, len(ordered))
	for _, item := range ordered {
		results = append(results, Result{Fact: facts[item.id].fact, Score: item.score, SourceRanks: ranks[item.id]})
	}
	return results
}

func budgetResults(results []Result, req RecallRequest, count TokenCounter, reranked bool) (RecallResult, error) {
	output := RecallResult{Results: []Result{}, Reranked: &reranked}
	// Count the serialized result array, including metadata and evidence framing.
	for _, result := range results {
		trial := append(append([]Result{}, output.Results...), result)
		b, err := json.Marshal(trial)
		if err != nil {
			return output, err
		}
		tokens := count(string(b))
		if tokens < 0 {
			return output, fmt.Errorf("negative token count")
		}
		if tokens > req.MaxTokens {
			continue
		}
		output.Results = trial
		output.EstimatedTokens = tokens
		if len(output.Results) == req.Limit {
			break
		}
	}
	return output, nil
}
