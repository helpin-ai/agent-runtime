package memory

// Semantic link construction follows pinned memories/pg/links.py: separate
// top-50 existing and within-batch neighbors at similarity >= 0.7. Local
// storage uses exact cosine scans in place of PostgreSQL's ANN index.
import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func createSemanticLinks(ctx context.Context, tx *sql.Tx, scope Scope, ids []string, vectors [][]float32, extracted []ExtractedFact) error {
	if len(ids) == 0 {
		return nil
	}
	newIDs := map[string]bool{}
	for _, id := range ids {
		newIDs[id] = true
	}
	type unit struct {
		id, kind string
		vector   []float32
	}
	units := []unit{}
	rows, err := tx.QueryContext(ctx, `SELECT id,payload,embedding FROM memory_facts WHERE app_id=? AND bank_id=?`, scope.AppID, scope.BankID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var u unit
		var payload, embedding string
		if err = rows.Scan(&u.id, &payload, &embedding); err != nil {
			rows.Close()
			return err
		}
		var fact Fact
		if err = json.Unmarshal([]byte(payload), &fact); err != nil {
			rows.Close()
			return err
		}
		u.kind = fact.Type
		if err = json.Unmarshal([]byte(embedding), &u.vector); err != nil {
			rows.Close()
			return err
		}
		if len(u.vector) != len(vectors[0]) {
			rows.Close()
			return fmt.Errorf("semantic link dimensions differ")
		}
		units = append(units, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		existing, batch := []ranked{}, []ranked{}
		for _, u := range units {
			if u.id == id || u.kind != extracted[i].Type {
				continue
			}
			similarity := cosine(vectors[i], u.vector)
			if similarity < 0.7 {
				continue
			}
			neighbor := ranked{u.id, min(1, similarity)}
			if newIDs[u.id] {
				batch = append(batch, neighbor)
			} else {
				existing = append(existing, neighbor)
			}
		}
		for _, neighbor := range append(top(existing, 50), top(batch, 50)...) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO memory_links(source_id,target_id,kind,weight) VALUES(?,?,'semantic',?) ON CONFLICT DO NOTHING`, id, neighbor.id, neighbor.score); err != nil {
				return err
			}
		}
	}
	return nil
}
