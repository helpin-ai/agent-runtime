package api

import (
	"net/http"
	"strings"
)

func (s *Server) browserControl(w http.ResponseWriter, r *http.Request) {
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	if appID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	agent, err := s.cfg.Store.GetAgent(r.Context(), appID, r.PathValue("agent_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agent == nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	action := "status"
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var input struct {
			Action string `json:"action"`
		}
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid browser control")
			return
		}
		action = input.Action
	}
	if s.cfg.Tools == nil {
		writeError(w, http.StatusServiceUnavailable, "browser tools are disabled")
		return
	}
	status, err := s.cfg.Tools.BrowserControl(r.Context(), agent, action)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}
