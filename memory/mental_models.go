package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type MentalModel struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	SourceQuery   string    `json:"source_query"`
	Content       string    `json:"content"`
	SourceFactIDs []string  `json:"source_fact_ids"`
	RefreshedAt   time.Time `json:"refreshed_at"`
	IsStale       bool      `json:"is_stale"`
}

// CreateMentalModel registers a host-curated question, then requires an explicit
// RefreshMentalModel call to derive its content. IDs and scope are host-selected.
func (s *SQLite) CreateMentalModel(ctx context.Context, scope Scope, id, name, query string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" || len(id) > 512 || strings.TrimSpace(name) == "" || len(name) > 512 {
		return fmt.Errorf("mental model ID and name required (maximum 512 bytes)")
	}
	if _, err := (RecallRequest{Query: query}).normalized(); err != nil {
		return err
	}
	o := MentalModel{ID: id, Name: name, SourceQuery: query, IsStale: true, SourceFactIDs: []string{}}
	payload, _ := json.Marshal(o)
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_mental_models(app_id,bank_id,id,payload,embedding) VALUES(?,?,?,?, '[]')`, scope.AppID, scope.BankID, id, string(payload))
	return err
}

func (s *SQLite) ReadMentalModel(ctx context.Context, scope Scope, id string) (MentalModel, error) {
	if err := scope.Validate(); err != nil {
		return MentalModel{}, err
	}
	var payload string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM memory_mental_models WHERE app_id=? AND bank_id=? AND id=?`, scope.AppID, scope.BankID, id).Scan(&payload); err != nil {
		return MentalModel{}, err
	}
	var out MentalModel
	err := json.Unmarshal([]byte(payload), &out)
	return out, err
}

func (s *SQLite) RefreshMentalModel(ctx context.Context, scope Scope, id string, model ReflectionModel) (MentalModel, error) {
	before, err := s.reflectionState(ctx, scope)
	if err != nil {
		return MentalModel{}, err
	}
	o, err := s.ReadMentalModel(ctx, scope, id)
	if err != nil {
		return MentalModel{}, err
	}
	// Refresh from observations/raw evidence, never from the model's own summary.
	answer, err := s.reflect(ctx, scope, ReflectRequest{Query: o.SourceQuery}, model, false)
	if err != nil {
		return MentalModel{}, err
	}
	if len(answer.SourceFactIDs) == 0 {
		return MentalModel{}, fmt.Errorf("mental model refresh requires source evidence")
	}
	vectors, err := s.cfg.Embedder.Embed(ctx, []string{o.Name + "\n" + o.SourceQuery + "\n" + answer.Answer})
	if err != nil {
		return MentalModel{}, err
	}
	if err = validateVectors(vectors, 1); err != nil {
		return MentalModel{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MentalModel{}, err
	}
	defer tx.Rollback()
	after, err := bankState(ctx, tx, scope)
	if err != nil {
		return MentalModel{}, err
	}
	if before != after {
		return MentalModel{}, ErrConflict
	}
	if err = s.ensureModel(ctx, tx, scope, len(vectors[0])); err != nil {
		return MentalModel{}, err
	}
	o.Content = answer.Answer
	o.SourceFactIDs = answer.SourceFactIDs
	o.RefreshedAt = time.Now().UTC()
	o.IsStale = false
	payload, _ := json.Marshal(o)
	vector, _ := json.Marshal(vectors[0])
	if _, err = tx.ExecContext(ctx, `UPDATE memory_mental_models SET payload=?,embedding=? WHERE app_id=? AND bank_id=? AND id=?`, string(payload), string(vector), scope.AppID, scope.BankID, id); err != nil {
		return MentalModel{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM memory_mental_model_proofs WHERE app_id=? AND bank_id=? AND model_id=?`, scope.AppID, scope.BankID, id); err != nil {
		return MentalModel{}, err
	}
	for _, factID := range o.SourceFactIDs {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_facts WHERE app_id=? AND bank_id=? AND id=?`, scope.AppID, scope.BankID, factID).Scan(&n); err != nil {
			return MentalModel{}, err
		}
		if n != 1 {
			return MentalModel{}, fmt.Errorf("mental model cites a source outside its bank")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO memory_mental_model_proofs(app_id,bank_id,model_id,fact_id) VALUES(?,?,?,?)`, scope.AppID, scope.BankID, id, factID); err != nil {
			return MentalModel{}, err
		}
	}
	return o, tx.Commit()
}

func (s *SQLite) SearchMentalModels(ctx context.Context, scope Scope, query string, limit int) ([]MentalModel, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 20 {
		return nil, fmt.Errorf("mental model limit must be 1–20")
	}
	if _, err := (RecallRequest{Query: query}).normalized(); err != nil {
		return nil, err
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
		return []MentalModel{}, nil
	}
	if err != nil {
		return nil, err
	}
	if model != s.cfg.Embedder.ModelID() || dims != len(vectors[0]) {
		return nil, fmt.Errorf("embedding model/dimensions differ from bank")
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload,embedding FROM memory_mental_models WHERE app_id=? AND bank_id=?`, scope.AppID, scope.BankID)
	if err != nil {
		return nil, err
	}
	type hit struct {
		model MentalModel
		score float64
	}
	hits := []hit{}
	for rows.Next() {
		var payload, embedding string
		if err = rows.Scan(&payload, &embedding); err != nil {
			break
		}
		var m MentalModel
		var v []float32
		if err = json.Unmarshal([]byte(payload), &m); err != nil {
			break
		}
		if m.IsStale || m.Content == "" {
			continue
		}
		if err = json.Unmarshal([]byte(embedding), &v); err != nil {
			break
		}
		if len(v) != dims {
			err = fmt.Errorf("stored mental model dimensions differ")
			break
		}
		if err = validateVectors([][]float32{v}, 1); err != nil {
			break
		}
		score := cosine(vectors[0], v)
		if score >= 0.3 {
			hits = append(hits, hit{m, score})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			return hits[i].model.ID < hits[j].model.ID
		}
		return hits[i].score > hits[j].score
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]MentalModel, len(hits))
	for i, h := range hits {
		out[i] = h.model
	}
	return out, tx.Commit()
}

func invalidateMentalModels(ctx context.Context, tx *sql.Tx, scope Scope, documentID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT m.id,m.payload FROM memory_mental_models m JOIN memory_mental_model_proofs p ON m.app_id=p.app_id AND m.bank_id=p.bank_id AND m.id=p.model_id JOIN memory_facts f ON f.id=p.fact_id WHERE f.app_id=? AND f.bank_id=? AND f.document_id=?`, scope.AppID, scope.BankID, documentID)
	if err != nil {
		return err
	}
	models := []MentalModel{}
	for rows.Next() {
		var id, payload string
		if err = rows.Scan(&id, &payload); err != nil {
			break
		}
		var m MentalModel
		if err = json.Unmarshal([]byte(payload), &m); err != nil {
			break
		}
		m.Content = ""
		m.SourceFactIDs = []string{}
		m.IsStale = true
		models = append(models, m)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, m := range models {
		payload, _ := json.Marshal(m)
		if _, err = tx.ExecContext(ctx, `UPDATE memory_mental_models SET payload=?,embedding='[]' WHERE app_id=? AND bank_id=? AND id=?`, string(payload), scope.AppID, scope.BankID, m.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM memory_mental_model_proofs WHERE app_id=? AND bank_id=? AND model_id=?`, scope.AppID, scope.BankID, m.ID); err != nil {
			return err
		}
	}
	return nil
}

// New raw evidence may change any generated summary. Until topic-level
// freshness tracking is ported, conservatively require explicit refresh.
func staleMentalModels(ctx context.Context, tx *sql.Tx, scope Scope) error {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM memory_mental_models WHERE app_id=? AND bank_id=?`, scope.AppID, scope.BankID)
	if err != nil {
		return err
	}
	models := []MentalModel{}
	for rows.Next() {
		var payload string
		if err = rows.Scan(&payload); err != nil {
			break
		}
		var m MentalModel
		if err = json.Unmarshal([]byte(payload), &m); err != nil {
			break
		}
		if !m.IsStale {
			m.IsStale = true
			models = append(models, m)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, m := range models {
		payload, _ := json.Marshal(m)
		if _, err = tx.ExecContext(ctx, `UPDATE memory_mental_models SET payload=? WHERE app_id=? AND bank_id=? AND id=?`, string(payload), scope.AppID, scope.BankID, m.ID); err != nil {
			return err
		}
	}
	return nil
}
