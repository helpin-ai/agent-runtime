package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// KeywordText mirrors upstream's text/context/entity/date full-text inputs.
// SQLite's tokenizer and BM25 score remain different from PostgreSQL ranking.
func KeywordText(fact Fact) string {
	signals := append([]string(nil), fact.Entities...)
	if fact.OccurredStart != nil {
		signals = append(signals, fact.OccurredStart.Format("January 2 2006"))
	}
	if fact.OccurredEnd != nil && (fact.OccurredStart == nil || !fact.OccurredEnd.Equal(*fact.OccurredStart)) {
		signals = append(signals, fact.OccurredEnd.Format("January 2 2006"))
	}
	return fact.Text + " " + fact.Context + " " + strings.Join(signals, " ")
}

func rebuildKeywordIndexes(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT row_id,app_id,bank_id,payload FROM memory_facts ORDER BY row_id")
	if err != nil {
		return err
	}
	type indexed struct {
		rowID int64
		scope Scope
		fact  Fact
	}
	var entries []indexed
	for rows.Next() {
		var entry indexed
		var payload string
		if err = rows.Scan(&entry.rowID, &entry.scope.AppID, &entry.scope.BankID, &payload); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal([]byte(payload), &entry.fact); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	banks := map[Scope]bool{}
	for _, entry := range entries {
		table := ftsTable(entry.scope)
		if !banks[entry.scope] {
			if _, err = tx.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "CREATE VIRTUAL TABLE "+table+" USING fts5(text, tokenize='porter unicode61')"); err != nil {
				return err
			}
			banks[entry.scope] = true
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO "+table+"(rowid,text) VALUES(?,?)", entry.rowID, KeywordText(entry.fact)); err != nil {
			return err
		}
	}
	return nil
}
