package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

type SQLiteConfig struct {
	Path        string
	Extractor   Extractor
	Embedder    Embedder
	Reranker    Reranker
	CountTokens TokenCounter
	// MaxObservations bounds each bank; zero leaves it unlimited.
	MaxObservations int
	// CandidateLimit bounds each retrieval arm before RRF (default 100).
	CandidateLimit int
	// RerankerMaxCandidates caps the fused pool before reranking and combined
	// scoring, matching Hindsight's default of 300. It is independent of the
	// per-arm retrieval limit: changing it also changes rank normalization.
	RerankerMaxCandidates int
}

type SQLite struct {
	db  *sql.DB
	cfg SQLiteConfig
}

// OpenSQLite uses the runtime CLI's existing pure-Go SQLite driver. Vector
// similarity is exact and computed in Go; no extension or embedding daemon is
// required by storage itself. Inference is supplied through the model interfaces.
func OpenSQLite(ctx context.Context, cfg SQLiteConfig) (*SQLite, error) {
	if cfg.Path == "" || cfg.Extractor == nil || cfg.Embedder == nil || cfg.Embedder.ModelID() == "" {
		return nil, fmt.Errorf("memory path, extractor, and identified embedder are required")
	}
	if cfg.MaxObservations < 0 {
		return nil, fmt.Errorf("maximum observations cannot be negative")
	}
	if cfg.CandidateLimit == 0 {
		cfg.CandidateLimit = 100
	}
	if cfg.CandidateLimit < 1 || cfg.CandidateLimit > 1000 {
		return nil, fmt.Errorf("candidate limit must be 1–1000")
	}
	if cfg.RerankerMaxCandidates == 0 {
		cfg.RerankerMaxCandidates = 300
	}
	if cfg.RerankerMaxCandidates < 1 || cfg.RerankerMaxCandidates > 10000 {
		return nil, fmt.Errorf("reranker candidate limit must be 1–10000")
	}
	if cfg.CountTokens == nil {
		cfg.CountTokens = countTokens
	}
	path, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &SQLite{db: db, cfg: cfg}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const schema = `
CREATE TABLE IF NOT EXISTS memory_schema (version INTEGER NOT NULL);
INSERT INTO memory_schema SELECT 6 WHERE NOT EXISTS (SELECT 1 FROM memory_schema);
CREATE TABLE IF NOT EXISTS memory_banks (
 app_id TEXT NOT NULL, bank_id TEXT NOT NULL, model_id TEXT NOT NULL, dimensions INTEGER NOT NULL,
 PRIMARY KEY(app_id, bank_id));
CREATE TABLE IF NOT EXISTS memory_documents (
 app_id TEXT NOT NULL, bank_id TEXT NOT NULL, document_id TEXT NOT NULL,
 revision INTEGER NOT NULL, digest TEXT NOT NULL, content TEXT NOT NULL,
 context TEXT NOT NULL, timestamp TEXT NOT NULL, metadata TEXT NOT NULL, forgotten INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(app_id, bank_id, document_id));
CREATE TABLE IF NOT EXISTS memory_facts (
 row_id INTEGER PRIMARY KEY, app_id TEXT NOT NULL, bank_id TEXT NOT NULL,
 id TEXT NOT NULL UNIQUE, document_id TEXT NOT NULL, payload TEXT NOT NULL, embedding TEXT NOT NULL,
 FOREIGN KEY(app_id, bank_id, document_id) REFERENCES memory_documents(app_id, bank_id, document_id));
CREATE TABLE IF NOT EXISTS memory_chunks (
 app_id TEXT NOT NULL, bank_id TEXT NOT NULL, id TEXT NOT NULL PRIMARY KEY,
 document_id TEXT NOT NULL, chunk_index INTEGER NOT NULL, text TEXT NOT NULL,
 FOREIGN KEY(app_id,bank_id,document_id) REFERENCES memory_documents(app_id,bank_id,document_id));
CREATE INDEX IF NOT EXISTS memory_chunks_scope ON memory_chunks(app_id,bank_id,document_id);
CREATE INDEX IF NOT EXISTS memory_facts_scope ON memory_facts(app_id, bank_id, document_id);
CREATE TABLE IF NOT EXISTS memory_entities (
 fact_id TEXT NOT NULL REFERENCES memory_facts(id) ON DELETE CASCADE, name TEXT NOT NULL,
 PRIMARY KEY(fact_id, name));
CREATE INDEX IF NOT EXISTS memory_entities_name ON memory_entities(name);
CREATE TABLE IF NOT EXISTS memory_links (
 source_id TEXT NOT NULL REFERENCES memory_facts(id) ON DELETE CASCADE,
 target_id TEXT NOT NULL REFERENCES memory_facts(id) ON DELETE CASCADE, kind TEXT NOT NULL, weight REAL NOT NULL DEFAULT 1,
 PRIMARY KEY(source_id, target_id, kind));
CREATE TABLE IF NOT EXISTS memory_observations (
 id TEXT PRIMARY KEY, app_id TEXT NOT NULL, bank_id TEXT NOT NULL,
 payload TEXT NOT NULL, embedding TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS memory_observations_scope ON memory_observations(app_id,bank_id);
CREATE TABLE IF NOT EXISTS memory_observation_proofs (
 observation_id TEXT NOT NULL REFERENCES memory_observations(id) ON DELETE CASCADE,
 fact_id TEXT NOT NULL REFERENCES memory_facts(id) ON DELETE CASCADE,
 PRIMARY KEY(observation_id,fact_id));
CREATE INDEX IF NOT EXISTS memory_observation_proofs_fact ON memory_observation_proofs(fact_id);
CREATE TABLE IF NOT EXISTS memory_consolidated (
 fact_id TEXT PRIMARY KEY REFERENCES memory_facts(id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS memory_mental_models (
 app_id TEXT NOT NULL, bank_id TEXT NOT NULL, id TEXT NOT NULL,
 payload TEXT NOT NULL, embedding TEXT NOT NULL, PRIMARY KEY(app_id,bank_id,id));
CREATE TABLE IF NOT EXISTS memory_mental_model_proofs (
 app_id TEXT NOT NULL, bank_id TEXT NOT NULL, model_id TEXT NOT NULL,
 fact_id TEXT NOT NULL REFERENCES memory_facts(id) ON DELETE CASCADE,
 FOREIGN KEY(app_id,bank_id,model_id) REFERENCES memory_mental_models(app_id,bank_id,id) ON DELETE CASCADE,
 PRIMARY KEY(app_id,bank_id,model_id,fact_id));
CREATE INDEX IF NOT EXISTS memory_mental_model_proofs_fact ON memory_mental_model_proofs(fact_id);
`
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("memory schema: %w", err)
	}
	var version int
	if err = tx.QueryRowContext(ctx, "SELECT version FROM memory_schema").Scan(&version); err != nil {
		return err
	}
	if version == 1 {
		if _, err = tx.ExecContext(ctx, "ALTER TABLE memory_links ADD COLUMN weight REAL NOT NULL DEFAULT 1"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE memory_schema SET version=2"); err != nil {
			return err
		}
		version = 2
	}
	if version == 2 {
		if err = migrateSourceChunks(ctx, tx); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE memory_schema SET version=3"); err != nil {
			return err
		}
		version = 3
	}
	if version == 3 {
		if err = rebuildKeywordIndexes(ctx, tx); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE memory_schema SET version=4"); err != nil {
			return err
		}
		version = 4
	}
	if version == 4 {
		if _, err = tx.ExecContext(ctx, "UPDATE memory_schema SET version=5"); err != nil {
			return err
		}
		version = 5
	}
	if version == 5 {
		if _, err = tx.ExecContext(ctx, "UPDATE memory_schema SET version=6"); err != nil {
			return err
		}
		version = 6
	}
	if version != 6 {
		return fmt.Errorf("unsupported memory schema version %d", version)
	}
	return tx.Commit()
}

func stableID(parts ...string) string {
	b, _ := json.Marshal(parts)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func requestDigest(r RetainRequest) string {
	b, _ := json.Marshal(r)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (s *SQLite) Retain(ctx context.Context, scope Scope, req RetainRequest) (RetainResult, error) {
	result := RetainResult{DocumentID: req.DocumentID}
	if err := scope.Validate(); err != nil {
		return result, err
	}
	if err := req.validate(); err != nil {
		return result, err
	}
	digest := requestDigest(req)
	var revision int64
	var oldDigest string
	var forgotten bool
	var oldCount int
	err := s.db.QueryRowContext(ctx, `SELECT revision,digest,forgotten,
 (SELECT count(*) FROM memory_facts f WHERE f.app_id=d.app_id AND f.bank_id=d.bank_id AND f.document_id=d.document_id)
 FROM memory_documents d WHERE app_id=? AND bank_id=? AND document_id=?`, scope.AppID, scope.BankID, req.DocumentID).Scan(&revision, &oldDigest, &forgotten, &oldCount)
	if err != nil && err != sql.ErrNoRows {
		return result, err
	}
	if forgotten {
		return result, ErrForgotten
	}
	if oldDigest == digest {
		result.Unchanged = true
		result.FactCount = oldCount
		return result, nil
	}
	if req.Timestamp.IsZero() {
		req.Timestamp = time.Now().UTC()
	}
	facts, err := s.cfg.Extractor.Extract(ctx, req)
	if err != nil {
		return result, fmt.Errorf("extract memory: %w", err)
	}
	if len(facts) > 2000 {
		return result, fmt.Errorf("extractor returned too many facts")
	}
	texts := make([]string, len(facts))
	for i, fact := range facts {
		if err = validateExtracted(fact, i); err != nil {
			return result, err
		}
		texts[i] = fact.Text
	}
	var vectors [][]float32
	if len(texts) > 0 {
		if augmented, ok := s.cfg.Embedder.(FactEmbedder); ok {
			vectors, err = augmented.EmbedFacts(ctx, facts, req.Timestamp)
		} else {
			vectors, err = s.cfg.Embedder.Embed(ctx, texts)
		}
		if err != nil {
			return result, fmt.Errorf("embed memory: %w", err)
		}
		if err = validateVectors(vectors, len(facts)); err != nil {
			return result, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var currentRevision int64
	var currentForgotten bool
	err = tx.QueryRowContext(ctx, `SELECT revision,forgotten FROM memory_documents WHERE app_id=? AND bank_id=? AND document_id=?`, scope.AppID, scope.BankID, req.DocumentID).Scan(&currentRevision, &currentForgotten)
	if err != nil && err != sql.ErrNoRows {
		return result, err
	}
	if currentForgotten {
		return result, ErrForgotten
	}
	if currentRevision != revision {
		return result, ErrConflict
	}
	if len(vectors) > 0 {
		if err = s.ensureModel(ctx, tx, scope, len(vectors[0])); err != nil {
			return result, err
		}
	}
	metadata, _ := json.Marshal(req.Metadata)
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_documents(app_id,bank_id,document_id,revision,digest,content,context,timestamp,metadata) VALUES(?,?,?,?,?,?,?,?,?)
 ON CONFLICT(app_id,bank_id,document_id) DO UPDATE SET revision=excluded.revision,digest=excluded.digest,content=excluded.content,context=excluded.context,timestamp=excluded.timestamp,metadata=excluded.metadata`, scope.AppID, scope.BankID, req.DocumentID, revision+1, digest, req.Content, req.Context, req.Timestamp.Format(time.RFC3339Nano), string(metadata))
	if err != nil {
		return result, err
	}
	if err = deleteFacts(ctx, tx, scope, req.DocumentID); err != nil {
		return result, err
	}
	// Bank-specific corpora prevent other users' documents influencing BM25.
	// Table identifiers are generated SHA-256 digests, never caller-supplied SQL.
	fts := ftsTable(scope)
	if _, err = tx.ExecContext(ctx, `CREATE VIRTUAL TABLE IF NOT EXISTS `+fts+` USING fts5(text, tokenize='porter unicode61')`); err != nil {
		return result, err
	}
	ids := make([]string, len(facts))
	chunkTexts := map[int]string{}
	for i, extracted := range facts {
		ids[i] = stableID(scope.AppID, scope.BankID, req.DocumentID, digest, fmt.Sprint(i))
		chunk := extracted.SourceChunk
		if chunk == nil {
			chunk = &ExtractedChunk{Index: 0, Text: req.Content}
		}
		if previous, ok := chunkTexts[chunk.Index]; ok && previous != chunk.Text {
			return result, fmt.Errorf("conflicting source chunk index")
		}
		chunkID := stableID(scope.AppID, scope.BankID, req.DocumentID, digest, "chunk", fmt.Sprint(chunk.Index))
		if _, exists := chunkTexts[chunk.Index]; !exists {
			if _, e := tx.ExecContext(ctx, `INSERT INTO memory_chunks(app_id,bank_id,id,document_id,chunk_index,text) VALUES(?,?,?,?,?,?)`, scope.AppID, scope.BankID, chunkID, req.DocumentID, chunk.Index, chunk.Text); e != nil {
				return result, e
			}
			chunkTexts[chunk.Index] = chunk.Text
		}
		fact := Fact{ID: ids[i], ChunkID: chunkID, DocumentID: req.DocumentID, Text: extracted.Text, Where: extracted.Where, Type: extracted.Type, Entities: extracted.Entities, Context: req.Context, MentionedAt: req.Timestamp, OccurredStart: extracted.OccurredStart, OccurredEnd: extracted.OccurredEnd, Metadata: req.Metadata}
		payload, _ := json.Marshal(fact)
		embedding, _ := json.Marshal(vectors[i])
		row, e := tx.ExecContext(ctx, `INSERT INTO memory_facts(app_id,bank_id,id,document_id,payload,embedding) VALUES(?,?,?,?,?,?)`, scope.AppID, scope.BankID, ids[i], req.DocumentID, string(payload), string(embedding))
		if e != nil {
			return result, e
		}
		rowID, e := row.LastInsertId()
		if e != nil {
			return result, e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO `+fts+`(rowid,text) VALUES(?,?)`, rowID, KeywordText(fact)); e != nil {
			return result, e
		}
		for _, entity := range extracted.Entities {
			name := normalizeEntity(entity)
			if name == "" {
				continue
			}
			if _, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_entities(fact_id,name) VALUES(?,?)`, ids[i], name); e != nil {
				return result, e
			}
		}
		for _, link := range extracted.CausalRelations {
			if _, e = tx.ExecContext(ctx, `INSERT INTO memory_links(source_id,target_id,kind) VALUES(?,?,?)`, ids[i], ids[link.TargetIndex], link.RelationType); e != nil {
				return result, e
			}
		}
	}
	if err = createSemanticLinks(ctx, tx, scope, ids, vectors, facts); err != nil {
		return result, err
	}
	if err = staleMentalModels(ctx, tx, scope); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	result.FactCount = len(facts)
	return result, nil
}

func validateExtracted(f ExtractedFact, index int) error {
	if chunk := f.SourceChunk; chunk != nil && (chunk.Index < 0 || chunk.Index > 8191 || strings.TrimSpace(chunk.Text) == "" || len(chunk.Text) > 256<<10) {
		return fmt.Errorf("invalid source chunk in fact %d", index)
	}
	if strings.TrimSpace(f.Text) == "" || len(f.Text) > 32768 || (f.Type != "world" && f.Type != "experience") || len(f.Entities) > 128 {
		return fmt.Errorf("invalid extracted fact %d", index)
	}
	for _, e := range f.Entities {
		if len(e) > 512 {
			return fmt.Errorf("entity too long in fact %d", index)
		}
	}
	if f.OccurredStart != nil && f.OccurredEnd != nil && f.OccurredEnd.Before(*f.OccurredStart) {
		return fmt.Errorf("invalid dates in fact %d", index)
	}
	for _, l := range f.CausalRelations {
		if l.TargetIndex < 0 || l.TargetIndex >= index || l.RelationType != "caused_by" {
			return fmt.Errorf("invalid causal link in fact %d", index)
		}
	}
	return nil
}

func validateVectors(v [][]float32, count int) error {
	if len(v) != count {
		return fmt.Errorf("embedding count mismatch")
	}
	dim := 0
	for _, vector := range v {
		if dim == 0 {
			dim = len(vector)
		}
		if dim == 0 || dim > 65536 || len(vector) != dim {
			return fmt.Errorf("invalid embedding dimensions")
		}
		norm := 0.0
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return fmt.Errorf("non-finite embedding")
			}
			norm += float64(value) * float64(value)
		}
		if norm == 0 {
			return fmt.Errorf("zero embedding")
		}
	}
	return nil
}

func (s *SQLite) ensureModel(ctx context.Context, tx *sql.Tx, scope Scope, dimensions int) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_banks(app_id,bank_id,model_id,dimensions) VALUES(?,?,?,?)`, scope.AppID, scope.BankID, s.cfg.Embedder.ModelID(), dimensions); err != nil {
		return err
	}
	var model string
	var dim int
	if err := tx.QueryRowContext(ctx, `SELECT model_id,dimensions FROM memory_banks WHERE app_id=? AND bank_id=?`, scope.AppID, scope.BankID).Scan(&model, &dim); err != nil {
		return err
	}
	if model != s.cfg.Embedder.ModelID() || dim != dimensions {
		return fmt.Errorf("embedding model/dimensions differ from bank; reindex into a new bank")
	}
	return nil
}

// Forget removes the document and all its facts, indexes, and graph links. A
// tombstone prevents an in-flight extraction or a retried import resurrecting it.
// Older snapshots must also be expired by the application's backup policy.
func (s *SQLite) Forget(ctx context.Context, scope Scope, documentID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(documentID) == "" || len(documentID) > 512 {
		return fmt.Errorf("document ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = deleteFacts(ctx, tx, scope, documentID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_documents(app_id,bank_id,document_id,revision,digest,content,context,timestamp,metadata,forgotten) VALUES(?,?,?,1,'','','','','{}',1)
 ON CONFLICT(app_id,bank_id,document_id) DO UPDATE SET revision=revision+1,digest='',content='',context='',timestamp='',metadata='{}',forgotten=1`, scope.AppID, scope.BankID, documentID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Snapshot creates a consistent standalone SQLite file using VACUUM INTO.
// The destination must not exist. Encrypt before uploading it to cloud storage.
func (s *SQLite) Snapshot(ctx context.Context, destination string) error {
	path, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	// SQLite accepts an existing, empty output file; reserving it sets private
	// permissions before any sensitive data is written.
	if _, err = s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func normalizeEntity(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func ftsTable(scope Scope) string { return "memory_fts_" + stableID(scope.AppID, scope.BankID) }

func deleteFacts(ctx context.Context, tx *sql.Tx, scope Scope, documentID string) error {
	if err := invalidateMentalModels(ctx, tx, scope, documentID); err != nil {
		return err
	}
	if err := invalidateObservations(ctx, tx, scope, documentID); err != nil {
		return err
	}
	fts := ftsTable(scope)
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, fts).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+fts+` WHERE rowid IN (SELECT row_id FROM memory_facts WHERE app_id=? AND bank_id=? AND document_id=?)`, scope.AppID, scope.BankID, documentID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM memory_facts WHERE app_id=? AND bank_id=? AND document_id=?`, scope.AppID, scope.BankID, documentID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM memory_chunks WHERE app_id=? AND bank_id=? AND document_id=?`, scope.AppID, scope.BankID, documentID)
	return err
}
