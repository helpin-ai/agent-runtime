package api

import (
	"net/http"
	"strconv"

	"github.com/helpin-ai/agent-runtime/internal/appconfig"
	"github.com/helpin-ai/agent-runtime/internal/engine"
)

type v2EventListResponse struct {
	Events              []engine.V2EventEnvelope `json:"events"`
	NextSequenceNo      int64                    `json:"next_sequence_no"`
	StreamStateSnapshot *v2StreamStateSnapshot   `json:"stream_state_snapshot,omitempty"`
}

type v2StreamStateSnapshot struct {
	SchemaVersion   string                 `json:"schema_version"`
	RunID           string                 `json:"run_id"`
	ThroughSequence int64                  `json:"through_sequence"`
	State           map[string]interface{} `json:"state"`
}

func (s *Server) listV2RunEvents(w http.ResponseWriter, r *http.Request, appID, runID string) {
	if !appconfig.UsesEventProtocolV2(s.cfg.AppConfig, appID) {
		writeError(w, http.StatusNotFound, "v2 event protocol is not enabled for app")
		return
	}
	events, ok := s.v2RunEvents(w, r, appID, runID)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after_sequence"), 10, 64)
	filtered := make([]engine.V2EventEnvelope, 0, len(events))
	for _, event := range events {
		if event.SequenceNo > after {
			filtered = append(filtered, event)
		}
	}
	response := v2EventListResponse{Events: filtered}
	if len(events) > 0 {
		response.NextSequenceNo = events[len(events)-1].SequenceNo
	}
	if after == 0 {
		response.StreamStateSnapshot = materializeV2Snapshot(runID, events)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getV2RunStreamState(w http.ResponseWriter, r *http.Request, appID, runID string) {
	if !appconfig.UsesEventProtocolV2(s.cfg.AppConfig, appID) {
		writeError(w, http.StatusNotFound, "v2 event protocol is not enabled for app")
		return
	}
	events, ok := s.v2RunEvents(w, r, appID, runID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, materializeV2Snapshot(runID, events))
}

func (s *Server) v2RunEvents(w http.ResponseWriter, r *http.Request, appID, runID string) ([]engine.V2EventEnvelope, bool) {
	run, err := s.cfg.Store.GetRun(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return nil, false
	}
	persisted, err := s.cfg.Store.ListEvents(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	events := make([]engine.V2EventEnvelope, 0, len(persisted))
	for _, event := range persisted {
		events = append(events, engine.V2EnvelopeFromPersisted(event))
	}
	return events, true
}

func materializeV2Snapshot(runID string, events []engine.V2EventEnvelope) *v2StreamStateSnapshot {
	through := int64(0)
	if len(events) > 0 {
		through = events[len(events)-1].SequenceNo
	}
	// The ordered canonical event list is deliberately retained in the first
	// v2 snapshot format. Consumers apply the same reducer for snapshots and
	// subsequent events, eliminating a second state interpretation.
	return &v2StreamStateSnapshot{
		SchemaVersion:   engine.EventSchemaVersionV2,
		RunID:           runID,
		ThroughSequence: through,
		State:           map[string]interface{}{"events": events},
	}
}
