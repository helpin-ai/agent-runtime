package api

import (
	"errors"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"net/http"
)

func (s *Server) runSteering(w http.ResponseWriter, r *http.Request, appID, runID string) {
	if r.Method == http.MethodGet {
		result, err := s.cfg.Engine.RunSteering(r.Context(), appID, runID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req sdk.SteerRunRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid steering request")
		return
	}
	result, err := s.cfg.Engine.SteerRun(r.Context(), appID, runID, req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, agentcore.ErrSteeringConflict) || errors.Is(err, agentcore.ErrSteeringTerminal) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}
