// Package memoryjobs owns durable background learning jobs. Hosts provide
// evidence/model callbacks and short-lived permission to use idle capacity.
package memoryjobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/sqliteuri"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/glebarez/go-sqlite"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/filelock"
)

type Callback struct{ URL, Token string }
type permission struct {
	policy  sdk.MemoryLearningPolicy
	expires time.Time
}

const extractionConcurrency = 3

type Service struct {
	db        *sql.DB
	lock      *os.File
	callbacks map[string]Callback
	client    *http.Client
	mu        sync.Mutex
	stepMu    sync.RWMutex
	claimMu   sync.Mutex
	closed    bool
	done      chan struct{}
	policies  map[string]permission
	active    map[string]context.CancelFunc
	notify    chan struct{}
	now       func() time.Time
}

func Open(ctx context.Context, path string, callbacks map[string]Callback) (*Service, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("memory job database path is required")
	}
	for app, callback := range callbacks {
		u, err := url.Parse(callback.URL)
		if app == "" || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || callback.Token == "" {
			return nil, errors.New("memory callbacks require an app, HTTP(S) URL and token")
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := filelock.Open(path + ".worker.lock")
	if err != nil {
		return nil, err
	}
	if err = filelock.TryLock(lock, true); err != nil {
		lock.Close()
		return nil, fmt.Errorf("cannot acquire memory job worker lock: %w", err)
	}
	opened := false
	defer func() {
		if !opened {
			lock.Close()
		}
	}()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	u := sqliteuri.File(path)
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS memory_learning_jobs (
 id TEXT PRIMARY KEY,app_id TEXT NOT NULL,bank_id TEXT NOT NULL,kind TEXT NOT NULL,source_id TEXT NOT NULL,
 revision TEXT NOT NULL,state TEXT NOT NULL,retry_at INTEGER NOT NULL DEFAULT 0,
 lease TEXT NOT NULL DEFAULT '',lease_until INTEGER NOT NULL DEFAULT 0,payload TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS memory_learning_jobs_queue ON memory_learning_jobs(state,retry_at);
 CREATE TABLE IF NOT EXISTS memory_learning_cooldowns(app_id TEXT NOT NULL,bank_id TEXT NOT NULL,last_success INTEGER NOT NULL,PRIMARY KEY(app_id,bank_id));`)
	if err != nil {
		db.Close()
		return nil, err
	}
	_, err = db.ExecContext(ctx, `UPDATE memory_learning_jobs SET state=CASE kind WHEN 'extraction' THEN 'queued' ELSE 'consolidation_pending' END,
 payload=json_set(payload,'$.state',CASE kind WHEN 'extraction' THEN 'queued' ELSE 'consolidation_pending' END),lease='',lease_until=0 WHERE state IN ('extracting','consolidating')`)
	if err != nil {
		db.Close()
		return nil, err
	}
	opened = true
	return &Service{db: db, lock: lock, callbacks: callbacks, client: &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, policies: map[string]permission{}, active: map[string]context.CancelFunc{}, notify: make(chan struct{}, 1), done: make(chan struct{}), now: time.Now}, nil
}

func (s *Service) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	for _, cancel := range s.active {
		cancel()
	}
	s.mu.Unlock()
	s.stepMu.Lock()
	defer s.stepMu.Unlock()
	err := s.db.Close()
	s.lock.Close()
	return err
}
func (s *Service) configured(app string) bool { _, ok := s.callbacks[app]; return ok }
func jobID(j sdk.MemoryLearningJob) string {
	body, _ := json.Marshal([]string{j.AppID, j.BankID, j.Kind, j.SourceID})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}
func retryMillis(at *time.Time) int64 {
	if at == nil {
		return 0
	}
	return at.UnixMilli()
}
func (s *Service) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Submit is idempotent for a source revision. A changed revision supersedes
// queued/running work without allowing the previous writer to finish it.
func (s *Service) Submit(ctx context.Context, j sdk.MemoryLearningJob) (sdk.MemoryLearningJob, error) {
	if !s.configured(j.AppID) || strings.TrimSpace(j.BankID) == "" || strings.TrimSpace(j.SourceID) == "" || strings.TrimSpace(j.Revision) == "" || len(j.BankID) > 256 || len(j.SourceID) > 512 || len(j.Revision) > 256 || j.Attempts < 0 || j.Attempts > 3 || (j.Kind != "extraction" && j.Kind != "consolidation") {
		return j, errors.New("invalid memory job or unconfigured app")
	}
	j.ID = jobID(j)
	if j.State == "" {
		if j.Kind == "extraction" {
			j.State = "queued"
		} else {
			j.State = "consolidation_pending"
		}
	}
	if j.State != "failed" && (j.Kind == "extraction" && j.State != "queued" || j.Kind == "consolidation" && j.State != "consolidation_pending" && j.State != "consolidated") {
		return j, errors.New("invalid initial memory job state")
	}
	payload, err := json.Marshal(j)
	if err != nil {
		return j, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return j, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_learning_jobs(id,app_id,bank_id,kind,source_id,revision,state,retry_at,payload) VALUES(?,?,?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,state=excluded.state,retry_at=excluded.retry_at,payload=excluded.payload,lease='',lease_until=0 WHERE memory_learning_jobs.revision<>excluded.revision`, j.ID, j.AppID, j.BankID, j.Kind, j.SourceID, j.Revision, j.State, retryMillis(j.RetryAt), string(payload))
	if err != nil {
		return j, err
	}
	if j.LastSuccess != nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_learning_cooldowns VALUES(?,?,?) ON CONFLICT(app_id,bank_id) DO UPDATE SET last_success=max(last_success,excluded.last_success)`, j.AppID, j.BankID, j.LastSuccess.UnixMilli())
		if err != nil {
			return j, err
		}
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT payload FROM memory_learning_jobs WHERE id=?`, j.ID).Scan(&raw); err != nil {
		return j, err
	}
	var out sdk.MemoryLearningJob
	if err = json.Unmarshal([]byte(raw), &out); err != nil {
		return j, err
	}
	if j.Manual && out.Kind == "consolidation" && !out.Manual {
		out.Manual = true
		raw, _ := json.Marshal(out)
		if _, err = tx.ExecContext(ctx, `UPDATE memory_learning_jobs SET payload=? WHERE id=? AND revision=?`, string(raw), out.ID, out.Revision); err != nil {
			return j, err
		}
	}
	if err = tx.Commit(); err != nil {
		return j, err
	}
	s.wake()
	return out, nil
}

func (s *Service) Jobs(ctx context.Context, app, bank string) ([]sdk.MemoryLearningJob, error) {
	if !s.configured(app) || bank == "" {
		return nil, errors.New("app_id and bank_id are required for a configured app")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM memory_learning_jobs WHERE app_id=? AND bank_id=? ORDER BY rowid`, app, bank)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sdk.MemoryLearningJob{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var j sdk.MemoryLearningJob
		if err = json.Unmarshal([]byte(raw), &j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Service) SetPolicy(p sdk.MemoryLearningPolicy) error {
	if !s.configured(p.AppID) {
		return errors.New("memory learning app is not configured")
	}
	s.mu.Lock()
	s.policies[p.AppID] = permission{p, s.now().Add(time.Minute)}
	if !p.AllowExtraction || !p.AllowConsolidation {
		for key, cancel := range s.active {
			if strings.HasPrefix(key, p.AppID+"/") && (!p.AllowExtraction || strings.HasSuffix(key, "/consolidation")) {
				cancel()
			}
		}
	}
	s.mu.Unlock()
	s.wake()
	return nil
}

func (s *Service) allowed(j sdk.MemoryLearningJob) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.policies[j.AppID]
	return !s.closed && s.now().Before(p.expires) && p.policy.AllowExtraction && (j.Kind == "extraction" || j.Manual || p.policy.AllowConsolidation)
}

func (s *Service) Retry(ctx context.Context, app, id string) error {
	if !s.configured(app) {
		return errors.New("memory learning app is not configured")
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM memory_learning_jobs WHERE app_id=? AND id=? AND state IN ('failed','cancelled')`, app, id).Scan(&raw); err != nil {
		return err
	}
	var j sdk.MemoryLearningJob
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		return err
	}
	j.Attempts, j.Error, j.RetryAt = 0, "", nil
	j.State = "queued"
	if j.Kind == "consolidation" {
		j.State = "consolidation_pending"
	}
	body, _ := json.Marshal(j)
	_, err := s.db.ExecContext(ctx, `UPDATE memory_learning_jobs SET state=?,retry_at=0,payload=? WHERE id=? AND app_id=? AND state IN ('failed','cancelled')`, j.State, string(body), id, app)
	s.wake()
	return err
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.notify:
		}
		var extractions sync.WaitGroup
		for n := 0; n < extractionConcurrency && ctx.Err() == nil; n++ {
			extractions.Add(1)
			go func() {
				defer extractions.Done()
				if _, err := s.step(ctx, "extraction"); err != nil {
					slog.ErrorContext(ctx, "memory extraction worker failed", "error", err)
				}
			}()
		}
		extractions.Wait()
		if _, err := s.step(ctx, "consolidation"); err != nil {
			slog.ErrorContext(ctx, "memory consolidation worker failed", "error", err)
		}
	}
}

func (s *Service) step(ctx context.Context, kind string) (bool, error) {
	if kind == "extraction" {
		s.stepMu.RLock()
		defer s.stepMu.RUnlock()
	} else {
		s.stepMu.Lock()
		defer s.stepMu.Unlock()
	}
	// Serialize claims until active registration; inference runs concurrently.
	s.claimMu.Lock()
	claiming := true
	defer func() {
		if claiming {
			s.claimMu.Unlock()
		}
	}()
	rows, err := s.db.QueryContext(ctx, `SELECT payload,state FROM memory_learning_jobs WHERE kind=? AND retry_at<=? AND (state IN ('queued','consolidation_pending') OR lease_until>0 AND lease_until<=?) ORDER BY rowid`, kind, s.now().UnixMilli(), s.now().UnixMilli())
	if err != nil {
		return false, err
	}
	var candidates []sdk.MemoryLearningJob
	for rows.Next() {
		var raw, state string
		if err = rows.Scan(&raw, &state); err != nil {
			break
		}
		var j sdk.MemoryLearningJob
		if err = json.Unmarshal([]byte(raw), &j); err != nil {
			break
		}
		candidates = append(candidates, j)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, j := range candidates {
		key := j.AppID + "/" + j.ID + "/" + kind
		if j.Manual {
			key += "/manual"
		}
		s.mu.Lock()
		_, running := s.active[key]
		s.mu.Unlock()
		if running {
			continue
		}
		if !s.allowed(j) {
			continue
		}
		if kind == "consolidation" {
			var last int64
			err = s.db.QueryRowContext(ctx, `SELECT last_success FROM memory_learning_cooldowns WHERE app_id=? AND bank_id=?`, j.AppID, j.BankID).Scan(&last)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
			if !j.Manual && last > 0 && s.now().Before(time.UnixMilli(last).Add(30*time.Minute)) {
				continue
			}
			var pending int
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM memory_learning_jobs WHERE app_id=? AND bank_id=? AND kind='extraction' AND state NOT IN ('indexed','no_lesson','cancelled','failed')`, j.AppID, j.BankID).Scan(&pending); err != nil {
				return false, err
			}
			if pending > 0 {
				continue
			}
		}
		lease := rand.Text()
		result, err := s.db.ExecContext(ctx, `UPDATE memory_learning_jobs SET state=?,lease=?,lease_until=? WHERE id=? AND revision=? AND (state IN ('queued','consolidation_pending') OR lease_until>0 AND lease_until<=?)`, processingState(kind), lease, s.now().Add(11*time.Minute).UnixMilli(), j.ID, j.Revision, s.now().UnixMilli())
		if err != nil {
			return false, err
		}
		claimed, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if claimed == 0 {
			continue
		}
		j.State = processingState(kind)
		raw, _ := json.Marshal(j)
		if _, err = s.db.ExecContext(ctx, `UPDATE memory_learning_jobs SET payload=? WHERE id=? AND lease=?`, string(raw), j.ID, lease); err != nil {
			return false, err
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		s.mu.Lock()
		s.active[key] = cancel
		s.mu.Unlock()
		s.claimMu.Unlock()
		claiming = false
		if !s.allowed(j) {
			cancel()
		}
		// Expired host permission also cancels a call when the host disappears.
		stop := make(chan struct{})
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-callCtx.Done():
					return
				case <-t.C:
					if !s.allowed(j) {
						cancel()
						return
					}
				}
			}
		}()
		out, callErr := s.execute(callCtx, j)
		interrupted := errors.Is(callCtx.Err(), context.Canceled)
		cancel()
		close(stop)
		s.mu.Lock()
		delete(s.active, key)
		s.mu.Unlock()
		j.Result = out
		j.RetryAt = nil
		j.Error = ""
		switch {
		case interrupted || out.Interrupted:
			j.State = pendingState(kind)
		case callErr != nil:
			j.Attempts++
			j.Error = callErr.Error()
			j.State = pendingState(kind)
			if j.Attempts >= 3 {
				j.State = "failed"
			} else {
				at := s.now().Add(time.Duration(j.Attempts*j.Attempts) * time.Minute)
				j.RetryAt = &at
			}
		case out.InvalidSource:
			j.State = "cancelled"
		case kind == "extraction":
			j.State = "indexed"
			if out.Facts == 0 {
				j.State = "no_lesson"
			}
		default:
			j.State = "consolidated"
			at := s.now().UTC()
			j.LastSuccess = &at
		}
		raw, _ = json.Marshal(j)
		writeCtx, stopWrite := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stopWrite()
		tx, err := s.db.BeginTx(writeCtx, nil)
		if err != nil {
			return true, err
		}
		defer tx.Rollback()
		result, err = tx.ExecContext(writeCtx, `UPDATE memory_learning_jobs SET state=?,retry_at=?,lease='',lease_until=0,payload=? WHERE id=? AND revision=? AND lease=?`, j.State, retryMillis(j.RetryAt), string(raw), j.ID, j.Revision, lease)
		if err != nil {
			return true, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return true, err
		}
		if changed > 0 && j.LastSuccess != nil && j.State == "consolidated" {
			if _, err = tx.ExecContext(writeCtx, `INSERT INTO memory_learning_cooldowns VALUES(?,?,?) ON CONFLICT(app_id,bank_id) DO UPDATE SET last_success=excluded.last_success`, j.AppID, j.BankID, j.LastSuccess.UnixMilli()); err != nil {
				return true, err
			}
		}
		err = tx.Commit()
		if err == nil {
			s.wake()
		}
		return true, err
	}
	return false, nil
}

func processingState(kind string) string {
	if kind == "extraction" {
		return "extracting"
	}
	return "consolidating"
}
func pendingState(kind string) string {
	if kind == "extraction" {
		return "queued"
	}
	return "consolidation_pending"
}

func (s *Service) execute(ctx context.Context, j sdk.MemoryLearningJob) (sdk.MemoryLearningResult, error) {
	var out sdk.MemoryLearningResult
	c := s.callbacks[j.AppID]
	body, _ := json.Marshal(j)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return out, errors.New("memory host callback unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return out, fmt.Errorf("memory host callback returned status %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return out, errors.New("invalid memory host response")
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, errors.New("invalid memory host response")
	}
	if out.Error != "" {
		message := strings.ReplaceAll(out.Error, c.Token, "[redacted]")
		if len(message) > 1000 {
			message = message[:1000]
		}
		return out, errors.New(message)
	}
	if out.Facts < 0 || out.Processed < 0 || out.Created < 0 || out.Updated < 0 || out.Deleted < 0 {
		return out, errors.New("invalid memory host result counts")
	}
	return out, nil
}
