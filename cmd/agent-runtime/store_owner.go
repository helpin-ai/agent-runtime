package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/storeowner"
)

// Startup states reported by /healthz before the runtime is ready. Values
// stay strings: hosts decode /healthz as a string map.
const (
	startupWaitingForStoreOwner = "waiting_for_store_owner"
	startupStarting             = "starting"
)

// startupServer serves /healthz while the runtime waits for main-store
// ownership and initializes, then hands every request to the real handler.
// Rex reads the waiting state and keeps waiting instead of restarting the
// runtime; every other route answers 503 until ready.
type startupServer struct {
	server  *http.Server
	handler atomic.Pointer[http.Handler]
	mu      sync.Mutex
	status  string
	owner   string
	errs    chan error
}

func startStartupServer(addr string) (*startupServer, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &startupServer{status: startupStarting, errs: make(chan error, 1)}
	s.server = newHTTPServer(addr, http.HandlerFunc(s.serve))
	go func() { s.errs <- s.server.Serve(listener) }()
	return s, nil
}

func (s *startupServer) serve(w http.ResponseWriter, r *http.Request) {
	if handler := s.handler.Load(); handler != nil {
		(*handler).ServeHTTP(w, r)
		return
	}
	s.mu.Lock()
	body := map[string]string{"status": s.status}
	if s.owner != "" {
		body["owner"] = s.owner
	}
	s.mu.Unlock()
	if r.URL.Path != "/healthz" {
		body = map[string]string{"error": "agent runtime is starting", "status": body["status"]}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *startupServer) setStatus(status, owner string) {
	s.mu.Lock()
	s.status, s.owner = status, owner
	s.mu.Unlock()
}

func (s *startupServer) ready(handler http.Handler) { s.handler.Store(&handler) }

// sqliteStoreOwnerPath returns the database path that needs a process owner,
// or "" when this configuration has none: non-SQLite and in-memory stores,
// and SQLite runtimes that neither recover runs nor run work jobs.
func sqliteStoreOwnerPath(getenv func(string) string, needsOwner bool) (string, error) {
	if !needsOwner {
		return "", nil
	}
	cfg, err := store.ResolveConfigFromEnv(getenv)
	if err != nil || cfg.InMemory || (cfg.Driver != "sqlite" && cfg.Driver != "sqlite3") {
		return "", err
	}
	return storeowner.DatabasePath(cfg.DSN)
}

// acquireStoreOwner waits for ownership of the SQLite main store, reporting
// the waiting state through the startup server. It never steals the lock or
// signals the current owner.
func acquireStoreOwner(ctx context.Context, path string, startup *startupServer) (*storeowner.Owner, error) {
	owner, err := storeowner.Acquire(ctx, path, func(current string) {
		startup.setStatus(startupWaitingForStoreOwner, current)
	})
	if err != nil {
		return nil, err
	}
	startup.setStatus(startupStarting, "")
	slog.Info("owns the runtime store", "database", path)
	return owner, nil
}
