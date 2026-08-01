package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/engine"
)

// runEvents streams a run's events to the client as Server-Sent Events. Each
// event is written as `event: <type>` with a JSON `data:` payload. A periodic
// comment line keeps the connection alive through proxies.
func (s *Server) runEvents(w http.ResponseWriter, r *http.Request, appID, runID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "event history unavailable")
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // belt-and-suspenders for proxies
	w.WriteHeader(http.StatusOK)

	var ch <-chan engine.Event
	unsubscribe := func() {}
	if s.cfg.Events != nil {
		ch, unsubscribe = s.cfg.Events.Subscribe(runID)
	}
	defer unsubscribe()

	// Open the stream so the client's onopen fires immediately.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	lastSequence := int64(0)
	flushPersisted := func() bool {
		events, err := s.cfg.Store.ListEvents(r.Context(), appID, runID)
		if err != nil {
			return true
		}
		for _, event := range events {
			if event.SequenceNo <= lastSequence {
				continue
			}
			if !writeSSEEvent(w, flusher, event.Type, event) {
				return false
			}
			lastSequence = event.SequenceNo
		}
		return true
	}
	if !flushPersisted() {
		return
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-poll.C:
			if !flushPersisted() {
				return
			}
		case event, open := <-ch:
			if ch != nil && !open {
				ch = nil
				continue
			}
			if engine.IsTransientLiveEvent(event.Type) {
				if !writeSSEEvent(w, flusher, event.Type, event) {
					return
				}
				continue
			}
			if !flushPersisted() {
				return
			}
		}
	}
}

func (s *Server) listRunEvents(w http.ResponseWriter, r *http.Request, appID, runID string) {
	run, err := s.cfg.Store.GetRun(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	events, err := s.cfg.Store.ListEvents(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, eventType string, value interface{}) bool {
	payload, err := json.Marshal(value)
	if err != nil {
		return true
	}
	if eventType == "" {
		eventType = "message"
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
