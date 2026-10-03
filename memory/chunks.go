package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func loadChunks(ctx context.Context, tx *sql.Tx, scope Scope) (map[string]SourceChunk, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,document_id,chunk_index,text FROM memory_chunks WHERE app_id=? AND bank_id=?`, scope.AppID, scope.BankID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chunks := map[string]SourceChunk{}
	for rows.Next() {
		var chunk SourceChunk
		if err := rows.Scan(&chunk.ID, &chunk.DocumentID, &chunk.Index, &chunk.Text); err != nil {
			return nil, err
		}
		chunks[chunk.ID] = chunk
	}
	return chunks, rows.Err()
}

func includeChunks(output *RecallResult, available map[string]SourceChunk, budget int, count TokenCounter) error {
	selected := map[string]SourceChunk{}
	measure := func(chunk SourceChunk) (int, error) {
		selected[chunk.ID] = chunk
		data, err := json.Marshal(selected)
		delete(selected, chunk.ID)
		if err != nil {
			return 0, err
		}
		n := count(string(data))
		if n < 0 {
			return 0, fmt.Errorf("negative source chunk token count")
		}
		return n, nil
	}
	for _, result := range output.Results {
		id := result.ChunkID
		if _, ok := selected[id]; ok {
			continue
		}
		chunk, ok := available[id]
		if !ok || chunk.Text == "" {
			continue
		}
		n, err := measure(chunk)
		if err != nil {
			return err
		}
		if n > budget {
			runes := []rune(chunk.Text)
			low, high := 0, len(runes)
			chunk.Truncated = true
			for low < high {
				mid := (low + high + 1) / 2
				chunk.Text = string(runes[:mid])
				trial, err := measure(chunk)
				if err != nil {
					return err
				}
				if trial <= budget {
					low = mid
				} else {
					high = mid - 1
				}
			}
			if low == 0 {
				continue
			}
			chunk.Text = string(runes[:low])
			n, err = measure(chunk)
			if err != nil {
				return err
			}
			if n > budget {
				continue
			}
		}
		selected[id] = chunk
		output.EstimatedChunkTokens = n
	}
	if len(selected) > 0 {
		output.Chunks = selected
	}
	return nil
}

// Earlier schemas preserved documents but lacked fact-to-chunk provenance.
// Preserve their raw source as one fallback chunk; no model calls on upgrade.
func migrateSourceChunks(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT app_id,bank_id,document_id,digest,content FROM memory_documents d WHERE forgotten=0 AND EXISTS(SELECT 1 FROM memory_facts f WHERE f.app_id=d.app_id AND f.bank_id=d.bank_id AND f.document_id=d.document_id AND json_extract(f.payload,'$.chunk_id') IS NULL)`)
	if err != nil {
		return err
	}
	type document struct{ app, bank, id, digest, content string }
	documents := []document{}
	for rows.Next() {
		var d document
		if err := rows.Scan(&d.app, &d.bank, &d.id, &d.digest, &d.content); err != nil {
			rows.Close()
			return err
		}
		documents = append(documents, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, d := range documents {
		id := stableID(d.app, d.bank, d.id, d.digest, "chunk", "0")
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_chunks(app_id,bank_id,id,document_id,chunk_index,text) VALUES(?,?,?,?,0,?)`, d.app, d.bank, id, d.id, d.content); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_facts SET payload=json_set(payload,'$.chunk_id',?) WHERE app_id=? AND bank_id=? AND document_id=? AND json_extract(payload,'$.chunk_id') IS NULL`, id, d.app, d.bank, d.id); err != nil {
			return err
		}
	}
	return nil
}
