package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// runEvents streams a run's events to the client as Server-Sent Events. Each
// event is written as `event: <type>` with a JSON `data:` payload. A periodic
// comment line keeps the connection alive through proxies.
func (s *Server) runEvents(w http.ResponseWriter, r *http.Request, runID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	if s.cfg.Events == nil {
		writeError(w, http.StatusServiceUnavailable, "event stream unavailable")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // belt-and-suspenders for proxies
	w.WriteHeader(http.StatusOK)

	ch, unsubscribe := s.cfg.Events.Subscribe(runID)
	defer unsubscribe()

	// Open the stream so the client's onopen fires immediately.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case event, open := <-ch:
			if !open {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			eventType := event.Type
			if eventType == "" {
				eventType = "message"
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload)
			flusher.Flush()
		}
	}
}
