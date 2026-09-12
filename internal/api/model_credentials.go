package api

import (
	"net/http"

	sdk "github.com/helpin-ai/agent-runtime-go"
)

func (s *Server) runModelCredential(w http.ResponseWriter, r *http.Request, appID, runID string) {
	switch r.Method {
	case http.MethodPut:
		var req sdk.UpdateRunModelCredentialRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid credential request")
			return
		}
		result, err := s.cfg.Engine.UpdateRunModelCredential(r.Context(), appID, runID, req.Credential)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodDelete:
		if err := s.cfg.Engine.RevokeRunModelCredential(r.Context(), appID, runID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
