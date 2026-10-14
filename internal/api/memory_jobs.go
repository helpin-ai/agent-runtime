package api

import (
	sdk "github.com/helpin-ai/agent-runtime-go"
	"net/http"
)

func (s *Server) memoryJobsAvailable(w http.ResponseWriter) bool {
	if s.cfg.MemoryJobs == nil {
		writeError(w, http.StatusServiceUnavailable, "memory learning is not configured")
		return false
	}
	return true
}
func (s *Server) submitMemoryJob(w http.ResponseWriter, r *http.Request) {
	if !s.memoryJobsAvailable(w) {
		return
	}
	var job sdk.MemoryLearningJob
	if err := decodeJSON(r, &job); err != nil {
		writeError(w, 400, "invalid memory job")
		return
	}
	out, err := s.cfg.MemoryJobs.Submit(r.Context(), job)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) listMemoryJobs(w http.ResponseWriter, r *http.Request) {
	if !s.memoryJobsAvailable(w) {
		return
	}
	out, err := s.cfg.MemoryJobs.Jobs(r.Context(), r.URL.Query().Get("app_id"), r.URL.Query().Get("bank_id"))
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) memoryLearningPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.memoryJobsAvailable(w) {
		return
	}
	var policy sdk.MemoryLearningPolicy
	if err := decodeJSON(r, &policy); err != nil {
		writeError(w, 400, "invalid memory learning policy")
		return
	}
	if err := s.cfg.MemoryJobs.SetPolicy(policy); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) retryMemoryJob(w http.ResponseWriter, r *http.Request) {
	if !s.memoryJobsAvailable(w) {
		return
	}
	if err := s.cfg.MemoryJobs.Retry(r.Context(), r.URL.Query().Get("app_id"), r.PathValue("id")); err != nil {
		writeError(w, 404, "memory job not found or not retryable")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
