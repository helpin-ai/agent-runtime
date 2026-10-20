package tools

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

//go:embed browser_extension/*
var browserExtensionFiles embed.FS

const browserExtensionID = "nfhnaomjebdbbmjhcfhpidkkkfjlkbga"
const browserExtensionOrigin = "chrome-extension://" + browserExtensionID
const browserExtensionTimeout = 35 * time.Second

type extensionCommand struct {
	ID      string   `json:"id"`
	Session string   `json:"session"`
	Args    []string `json:"args"`
	Epoch   string   `json:"epoch"`
	Expires int64    `json:"expires"`
	reply   chan json.RawMessage
	done    <-chan struct{}
}

type extensionBrowser struct {
	// gate serializes actions with takeover, so user and agent never act together.
	gate      sync.Mutex
	mu        sync.Mutex
	token     string
	epoch     string
	queue     chan *extensionCommand
	pending   map[string]*extensionCommand
	seen      time.Time
	connected chan struct{}
	process   *exec.Cmd
	exited    chan struct{}
	profile   string
	page      string
	dir       string
	agentID   string
	held      bool
	runID     string
	idleTimer *time.Timer
}

func (e *extensionBrowser) waitConnected(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, browserExtensionTimeout)
	defer cancel()
	e.mu.Lock()
	seen, connected := e.seen, e.connected
	e.mu.Unlock()
	if !seen.IsZero() {
		return nil
	}
	select {
	case <-connected:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("browser extension did not connect during startup; no command was sent: %w", ctx.Err())
	}
}

func (e *extensionBrowser) execute(ctx context.Context, session string, args []string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, browserExtensionTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	command := &extensionCommand{ID: rand.Text(), Epoch: e.epoch, Session: session, Args: args, Expires: deadline.UnixMilli(), reply: make(chan json.RawMessage, 1), done: ctx.Done()}
	e.mu.Lock()
	e.pending[command.ID] = command
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.pending, command.ID); e.mu.Unlock() }()
	select {
	case e.queue <- command:
	case <-ctx.Done():
		return nil, errors.New("Browser extension did not connect; inspect the browser before retrying")
	}
	select {
	case raw := <-command.reply:
		return raw, nil
	case <-ctx.Done():
		// Cancellation cannot retract an action already delivered to the browser.
		// Keep the takeover gate until its deadline or acknowledgement.
		timer := time.NewTimer(max(time.Until(deadline), 0))
		defer timer.Stop()
		select {
		case <-command.reply:
		case <-timer.C:
		}
		return nil, errors.New("extension command timed out; check the page before retrying an action")
	}
}

// extensionBridge accepts only a bot-specific credential from extension context.
// Web pages get neither this credential nor a message channel into the driver.
func (m *BrowserManager) extensionBridge(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != browserExtensionOrigin {
		extensionError(w, 403, errors.New("invalid extension origin"))
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var ext *extensionBrowser
	m.mu.Lock()
	for _, candidate := range m.extensions {
		if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(candidate.token)) == 1 {
			ext = candidate
			break
		}
	}
	m.mu.Unlock()
	if ext == nil {
		extensionError(w, 401, errors.New("unauthorized"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		ext.mu.Lock()
		if ext.seen.IsZero() && ext.connected != nil {
			close(ext.connected)
		}
		ext.seen = time.Now()
		ext.mu.Unlock()
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		for {
			select {
			case command := <-ext.queue:
				select {
				case <-command.done:
					continue
				default:
				}
				extensionJSON(w, 200, command)
				return
			case <-timer.C:
				w.WriteHeader(204)
				return
			case <-r.Context().Done():
				return
			}
		}
	}
	var input struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&input); err != nil {
		extensionError(w, 400, errors.New("invalid extension response"))
		return
	}
	var result struct {
		Success *bool `json:"success"`
	}
	if json.Unmarshal(input.Result, &result) != nil || result.Success == nil {
		extensionError(w, 400, errors.New("invalid extension result"))
		return
	}
	ext.mu.Lock()
	command := ext.pending[input.ID]
	delete(ext.pending, input.ID)
	ext.mu.Unlock()
	if command == nil {
		extensionError(w, 409, errors.New("expired extension command"))
		return
	}
	select {
	case <-command.done:
		extensionError(w, 409, errors.New("expired extension command"))
	case command.reply <- input.Result:
		w.WriteHeader(204)
	default:
		extensionError(w, 409, errors.New("duplicate extension result"))
	}
}

func extensionJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func extensionError(w http.ResponseWriter, status int, err error) {
	extensionJSON(w, status, map[string]string{"error": err.Error()})
}
