package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Observation is derived evidence. SourceFactIDs are mandatory provenance, not
// model-selected permissions. Any changed/forgotten proof invalidates the text.
type Observation struct {
	ID             string     `json:"id"`
	Text           string     `json:"text"`
	SourceFactIDs  []string   `json:"source_fact_ids"`
	ProofCount     int        `json:"proof_count"`
	UpdatedAt      time.Time  `json:"updated_at"`
	IsStale        bool       `json:"is_stale"`
	MentionedAt    time.Time  `json:"mentioned_at"`
	OccurredStart  *time.Time `json:"occurred_start,omitempty"`
	OccurredEnd    *time.Time `json:"occurred_end,omitempty"`
	SourceMemories []Fact     `json:"source_memories,omitempty"`
}

type ConsolidationAction struct {
	ObservationID string   `json:"observation_id,omitempty"`
	Text          string   `json:"text"`
	SourceFactIDs []string `json:"source_fact_ids"`
	Reason        string   `json:"reason"`
}
type ConsolidationDelete struct {
	ObservationID string `json:"observation_id"`
	Reason        string `json:"reason"`
}
type ConsolidationPlan struct {
	Creates []ConsolidationAction `json:"creates"`
	Updates []ConsolidationAction `json:"updates"`
	Deletes []ConsolidationDelete `json:"deletes"`
}
type Consolidator interface {
	Consolidate(context.Context, []Fact, []Observation) (ConsolidationPlan, error)
}
type ConsolidationResult struct {
	Processed int `json:"processed"`
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Deleted   int `json:"deleted"`
}

// ConsolidateMemory uses the configured provider, letting a host schedule work
// without extracting model credentials from the initialized backend.
func (s *SQLite) ConsolidateMemory(ctx context.Context, scope Scope, limit int) (ConsolidationResult, error) {
	model, ok := s.cfg.Extractor.(Consolidator)
	if !ok {
		return ConsolidationResult{}, fmt.Errorf("configured extractor does not support consolidation")
	}
	for {
		result, err := s.Consolidate(ctx, scope, model, limit)
		if err == nil || !errors.Is(err, errConsolidationOutput) || limit <= 1 {
			return result, err
		}
		limit = max(1, limit/2)
	}
}

// Consolidate processes a bounded batch of previously unprocessed facts. The
// host schedules this operation; retain never starts background model calls.
// All inference finishes outside transactions. A bank-state CAS prevents stale
// inference from restoring information after source replacement or forgetting.
func (s *SQLite) Consolidate(ctx context.Context, scope Scope, model Consolidator, limit int) (out ConsolidationResult, err error) {
	if err = scope.Validate(); err != nil {
		return
	}
	if model == nil || limit < 1 || limit > 100 {
		return out, fmt.Errorf("consolidator and batch size 1–100 required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	before, err := bankState(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM memory_facts f WHERE app_id=? AND bank_id=? AND NOT EXISTS(SELECT 1 FROM memory_consolidated c WHERE c.fact_id=f.id) ORDER BY row_id LIMIT ?`, scope.AppID, scope.BankID, limit)
	if err != nil {
		return out, err
	}
	facts := []Fact{}
	allowed := map[string]Fact{}
	for rows.Next() {
		var payload string
		if err = rows.Scan(&payload); err != nil {
			break
		}
		var f Fact
		if err = json.Unmarshal([]byte(payload), &f); err != nil {
			break
		}
		facts = append(facts, f)
		allowed[f.ID] = f
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	observations, err := readObservations(ctx, tx, scope)
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	if len(facts) == 0 {
		return out, nil
	}
	bankObservations := observations
	// Match upstream's per-fact related-observation recall and union, rather
	// than exposing every observation in the bank to every batch.
	union := []Observation{}
	seenObservations := map[string]bool{}
	perFactObservations := map[string]map[string]bool{}
	for _, f := range facts {
		found, e := s.searchObservations(ctx, scope, f.Text, 100, true)
		if e != nil {
			return out, e
		}
		perFactObservations[f.ID] = map[string]bool{}
		used := 0
		for _, o := range found {
			encoded, _ := json.Marshal(struct {
				Text string `json:"text"`
			}{o.Text})
			size := s.cfg.CountTokens(string(encoded))
			if used+size > 512 {
				continue
			}
			used += size
			perFactObservations[f.ID][o.ID] = true
			if !seenObservations[o.ID] {
				seenObservations[o.ID] = true
				union = append(union, o)
			}
		}
	}
	observations = union
	// Source proofs are prompt evidence, not just identifiers. Read them scoped
	// and let the final bank-state CAS reject any concurrent replacement/forget.
	for i := range observations {
		used := 0
		for _, id := range observations[i].SourceFactIDs {
			var payload string
			if err = s.db.QueryRowContext(ctx, `SELECT payload FROM memory_facts WHERE app_id=? AND bank_id=? AND id=?`, scope.AppID, scope.BankID, id).Scan(&payload); err != nil {
				return out, err
			}
			var f Fact
			if err = json.Unmarshal([]byte(payload), &f); err != nil {
				return out, err
			}
			size := s.cfg.CountTokens(f.Text + f.Context)
			if used+size > 256 {
				continue
			}
			used += size
			observations[i].SourceMemories = append(observations[i].SourceMemories, f)
		}
	}
	old := map[string]Observation{}
	for _, o := range observations {
		old[o.ID] = o
	}
	var plan ConsolidationPlan
	if capacityModel, ok := model.(interface {
		consolidateWithCapacity(context.Context, []Fact, []Observation, string) (ConsolidationPlan, error)
	}); ok && s.cfg.MaxObservations > 0 {
		remaining := max(0, s.cfg.MaxObservations-len(bankObservations))
		note := fmt.Sprintf("This bank has %d observation slots remaining (out of %d). Prefer UPDATE over CREATE.", remaining, s.cfg.MaxObservations)
		if remaining == 0 {
			note += " Only UPDATE or DELETE existing observations; do not create new ones."
		}
		plan, err = capacityModel.consolidateWithCapacity(ctx, facts, observations, note)
	} else {
		plan, err = model.Consolidate(ctx, facts, observations)
	}
	if err != nil {
		return out, err
	}
	if len(plan.Creates)+len(plan.Updates)+len(plan.Deletes) > 200 {
		return out, fmt.Errorf("too many consolidation actions")
	}
	// Upstream drops creates that repeat a shown observation or an update.
	// Normalize whitespace only: casing can distinguish names and identifiers.
	normalize := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	existingTexts := map[string]bool{}
	for _, o := range observations {
		existingTexts[normalize(o.Text)] = true
	}
	for _, update := range plan.Updates {
		existingTexts[normalize(update.Text)] = true
	}
	prepared := []Observation{}
	seen := map[string]bool{}
	for i, action := range append(append([]ConsolidationAction{}, plan.Creates...), plan.Updates...) {
		isUpdate := i >= len(plan.Creates)
		o := Observation{Text: strings.TrimSpace(action.Text)}
		if o.Text == "" || len(o.Text) > 32768 || len(action.SourceFactIDs) == 0 || len(action.SourceFactIDs) > 100 {
			return out, fmt.Errorf("invalid observation action")
		}
		proofs := map[string]bool{}
		if isUpdate {
			var ok bool
			o, ok = old[action.ObservationID]
			if !ok || seen[o.ID] {
				return out, fmt.Errorf("unknown or duplicate update target")
			}
			// An update must cite at least one new fact that actually retrieved
			// its target, matching upstream's per-fact target validation.
			retrieved := false
			for _, id := range action.SourceFactIDs {
				retrieved = retrieved || perFactObservations[id][o.ID]
			}
			if !retrieved {
				return out, fmt.Errorf("update target was not retrieved for its source facts")
			}
			seen[o.ID] = true
			o.Text = strings.TrimSpace(action.Text)
			for _, id := range o.SourceFactIDs {
				proofs[id] = true
			}
		} else {
			if action.ObservationID != "" {
				return out, fmt.Errorf("create cannot choose observation ID")
			}
			o.ID = stableID(scope.AppID, scope.BankID, "observation", before, fmt.Sprint(i), o.Text)
		}
		for _, id := range action.SourceFactIDs {
			if _, ok := allowed[id]; !ok {
				return out, fmt.Errorf("observation cites a fact outside the supplied batch")
			}
			proofs[id] = true
		}
		o.SourceFactIDs = nil
		for id := range proofs {
			o.SourceFactIDs = append(o.SourceFactIDs, id)
		}
		sort.Strings(o.SourceFactIDs)
		o.SourceMemories = nil
		if !isUpdate {
			if existingTexts[normalize(o.Text)] {
				continue
			}
			existingTexts[normalize(o.Text)] = true
		}
		prepared = append(prepared, o)
	}
	for _, action := range plan.Deletes {
		if _, ok := old[action.ObservationID]; !ok || seen[action.ObservationID] {
			return out, fmt.Errorf("unknown or duplicate delete target")
		}
		seen[action.ObservationID] = true
	}
	texts := make([]string, len(prepared))
	for i, o := range prepared {
		texts[i] = o.Text
	}
	vectors := [][]float32{}
	if len(texts) > 0 {
		vectors, err = s.cfg.Embedder.Embed(ctx, texts)
		if err != nil {
			return out, err
		}
		if err = validateVectors(vectors, len(texts)); err != nil {
			return out, err
		}
	}
	prepared, vectors, foldedDeletes, err := s.deduplicateObservations(ctx, scope, model, prepared, vectors, bankObservations)
	if err != nil {
		return out, err
	}
	for _, id := range foldedDeletes {
		plan.Deletes = append(plan.Deletes, ConsolidationDelete{ObservationID: id, Reason: "semantic dedup fold"})
	}
	for _, action := range plan.Deletes {
		for _, o := range prepared {
			if action.ObservationID == o.ID {
				return out, fmt.Errorf("dedup target also scheduled for deletion")
			}
		}
	}
	if s.cfg.MaxObservations > 0 {
		ids := map[string]bool{}
		for _, o := range bankObservations {
			ids[o.ID] = true
		}
		for _, action := range plan.Deletes {
			delete(ids, action.ObservationID)
		}
		for _, o := range prepared {
			ids[o.ID] = true
		}
		if len(ids) > s.cfg.MaxObservations {
			return out, fmt.Errorf("%w: observation capacity exceeded", errConsolidationOutput)
		}
	}
	write, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer write.Rollback()
	after, err := bankState(ctx, write, scope)
	if err != nil {
		return out, err
	}
	if before != after {
		return out, ErrConflict
	}
	for _, action := range plan.Deletes {
		if _, err = write.ExecContext(ctx, `DELETE FROM memory_observations WHERE app_id=? AND bank_id=? AND id=?`, scope.AppID, scope.BankID, action.ObservationID); err != nil {
			return out, err
		}
	}
	for i, o := range prepared {
		if err = s.ensureModel(ctx, write, scope, len(vectors[i])); err != nil {
			return out, err
		}
		// Re-read all accumulated proofs, including those from older batches.
		for _, id := range o.SourceFactIDs {
			var payload string
			if err = write.QueryRowContext(ctx, `SELECT payload FROM memory_facts WHERE app_id=? AND bank_id=? AND id=?`, scope.AppID, scope.BankID, id).Scan(&payload); err != nil {
				return out, err
			}
			var f Fact
			if err = json.Unmarshal([]byte(payload), &f); err != nil {
				return out, err
			}
			if f.MentionedAt.After(o.MentionedAt) {
				o.MentionedAt = f.MentionedAt
			}
			if f.OccurredStart != nil && (o.OccurredStart == nil || f.OccurredStart.Before(*o.OccurredStart)) {
				o.OccurredStart = f.OccurredStart
			}
			if f.OccurredEnd != nil && (o.OccurredEnd == nil || f.OccurredEnd.After(*o.OccurredEnd)) {
				o.OccurredEnd = f.OccurredEnd
			}
		}
		o.ProofCount = len(o.SourceFactIDs)
		o.UpdatedAt = time.Now().UTC()
		o.IsStale = false
		payload, _ := json.Marshal(o)
		vector, _ := json.Marshal(vectors[i])
		if _, err = write.ExecContext(ctx, `INSERT INTO memory_observations(id,app_id,bank_id,payload,embedding) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,embedding=excluded.embedding`, o.ID, scope.AppID, scope.BankID, string(payload), string(vector)); err != nil {
			return out, err
		}
		for _, id := range o.SourceFactIDs {
			if _, err = write.ExecContext(ctx, `INSERT OR IGNORE INTO memory_observation_proofs(observation_id,fact_id) VALUES(?,?)`, o.ID, id); err != nil {
				return out, err
			}
		}
	}
	for _, f := range facts {
		if _, err = write.ExecContext(ctx, `INSERT INTO memory_consolidated(fact_id) VALUES(?)`, f.ID); err != nil {
			return out, err
		}
	}
	if err = write.Commit(); err != nil {
		return out, err
	}
	created, updated := 0, 0
	existing := map[string]bool{}
	for _, o := range bankObservations {
		existing[o.ID] = true
	}
	for _, o := range prepared {
		if existing[o.ID] {
			updated++
		} else {
			created++
		}
	}
	return ConsolidationResult{len(facts), created, updated, len(plan.Deletes)}, nil
}

func bankState(ctx context.Context, tx *sql.Tx, scope Scope) (string, error) {
	parts := []string{}
	for _, query := range []string{`SELECT document_id,revision FROM memory_documents WHERE app_id=? AND bank_id=? ORDER BY document_id`, `SELECT id,payload FROM memory_observations WHERE app_id=? AND bank_id=? ORDER BY id`, `SELECT f.id,c.fact_id FROM memory_facts f JOIN memory_consolidated c ON f.id=c.fact_id WHERE app_id=? AND bank_id=? ORDER BY f.id`, `SELECT id,payload FROM memory_mental_models WHERE app_id=? AND bank_id=? ORDER BY id`} {
		rows, err := tx.QueryContext(ctx, query, scope.AppID, scope.BankID)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var a, b string
			if err = rows.Scan(&a, &b); err != nil {
				rows.Close()
				return "", err
			}
			parts = append(parts, a, b)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
	}
	return stableID(parts...), nil
}
func readObservations(ctx context.Context, tx *sql.Tx, scope Scope) ([]Observation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM memory_observations WHERE app_id=? AND bank_id=? ORDER BY id`, scope.AppID, scope.BankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Observation{}
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			return nil, err
		}
		var o Observation
		if err = json.Unmarshal([]byte(p), &o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func invalidateObservations(ctx context.Context, tx *sql.Tx, scope Scope, documentID string) error {
	// Invalidate the whole derived sentence, rather than keep text potentially
	// learned from a removed proof. Surviving proofs become eligible to rebuild.
	const affected = `SELECT p.observation_id FROM memory_observation_proofs p JOIN memory_facts f ON p.fact_id=f.id WHERE f.app_id=? AND f.bank_id=? AND f.document_id=?`
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_consolidated WHERE fact_id IN (SELECT fact_id FROM memory_observation_proofs WHERE observation_id IN (`+affected+`))`, scope.AppID, scope.BankID, documentID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM memory_observations WHERE id IN (`+affected+`)`, scope.AppID, scope.BankID, documentID)
	return err
}
