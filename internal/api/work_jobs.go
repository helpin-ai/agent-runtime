package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/helpin-ai/agent-runtime-go"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/workjobs"
)

const (
	maxWorkJobBody      = 32 << 20
	maxWorkJobSmallBody = 1 << 20
	workJobStreamPoll   = 500 * time.Millisecond
)

// workJobsCapability reports work job availability for one app.
func (s *Server) workJobsCapability(appID string) *sdk.FeatureCapability {
	appID = strings.TrimSpace(appID)
	if appID == "" || s.cfg.WorkJobs == nil {
		return nil
	}
	supported, reason := s.cfg.WorkJobs.Available(appID)
	return &sdk.FeatureCapability{Supported: supported, Reason: reason}
}

// workJobsAllowed answers before any job SQL runs. Apps without the
// capability get 404, as if the routes did not exist; an opted-in app on a
// store without work jobs gets an explicit unsupported answer.
func (s *Server) workJobsAllowed(w http.ResponseWriter, appID string) bool {
	if s.cfg.WorkJobs == nil {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	ok, reason := s.cfg.WorkJobs.Available(appID)
	if ok {
		return true
	}
	if reason == workjobs.ReasonUnsupportedStore && s.workJobsOptedIn(appID) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": workjobs.ErrUnsupported.Error(), "reason": reason})
		return false
	}
	writeError(w, http.StatusNotFound, "not found")
	return false
}

func (s *Server) workJobsOptedIn(appID string) bool {
	if s.cfg.AppConfig == nil {
		return false
	}
	for _, app := range s.cfg.AppConfig.Apps {
		if app.AppID == appID {
			return app.WorkJobs
		}
	}
	return false
}

func (s *Server) workJobsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createWorkJob(w, r)
	case http.MethodGet:
		appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
		if appID == "" {
			writeError(w, http.StatusBadRequest, "app_id is required")
			return
		}
		if !s.workJobsAllowed(w, appID) {
			return
		}
		jobs, err := s.cfg.WorkJobs.List(r.Context(), appID)
		if err == nil && jobs == nil {
			jobs = []agentcore.WorkJob{}
		}
		writeWorkJobResult(w, http.StatusOK, jobs, err)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) createWorkJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkJobBody)
	var req sdk.CreateWorkJobRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.AppID = strings.TrimSpace(req.AppID)
	if req.AppID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	if !s.workJobsAllowed(w, req.AppID) {
		return
	}
	job, err := s.cfg.WorkJobs.Create(r.Context(), req)
	writeWorkJobResult(w, http.StatusCreated, job, err)
}

// workJobSubroutes serves /v1/work-jobs/{job_id}[/items|/attempts|/events|
// /events/stream|/dataset|/pause|/resume|/cancel|/revise].
func (s *Server) workJobSubroutes(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/work-jobs/"), "/"), "/")
	jobID, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(jobID) == "" || len(parts) > 3 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	if appID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	if !s.workJobsAllowed(w, appID) {
		return
	}
	action := strings.Join(parts[1:], "/")
	ctx := r.Context()
	query := r.URL.Query()
	jobs := s.cfg.WorkJobs
	switch {
	case action == "" && r.Method == http.MethodGet:
		job, err := jobs.Get(ctx, appID, jobID)
		writeWorkJobResult(w, http.StatusOK, job, err)
	case action == "items" && r.Method == http.MethodGet:
		limit, err := strconv.Atoi(defaultString(query.Get("limit"), "0"))
		if err != nil || limit < 0 || limit > 1000 {
			writeError(w, http.StatusBadRequest, "limit must be between 0 and 1000")
			return
		}
		page, err := jobs.Items(ctx, appID, jobID, query.Get("state"), query.Get("after"), limit)
		writeWorkJobResult(w, http.StatusOK, page, err)
	case action == "items" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxWorkJobBody)
		var req sdk.AddWorkItemsRequest
		if err := decodeStrictJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		job, err := jobs.AddItems(ctx, appID, jobID, req)
		writeWorkJobResult(w, http.StatusOK, job, err)
	case action == "attempts" && r.Method == http.MethodGet:
		attempts, err := jobs.Attempts(ctx, appID, jobID)
		if err == nil && attempts == nil {
			attempts = []agentcore.WorkAttempt{}
		}
		writeWorkJobResult(w, http.StatusOK, attempts, err)
	case action == "events" && r.Method == http.MethodGet:
		after, afterErr := parseOptionalInt64(query.Get("after"))
		limit, limitErr := parseOptionalInt64(query.Get("limit"))
		if afterErr != nil || limitErr != nil || after < 0 || limit < 0 || limit > 500 {
			writeError(w, http.StatusBadRequest, "after and limit must be non-negative integers; limit is at most 500")
			return
		}
		page, err := jobs.Events(ctx, appID, jobID, after, int(limit))
		writeWorkJobResult(w, http.StatusOK, page, err)
	case action == "events/stream" && r.Method == http.MethodGet:
		s.streamWorkJobEvents(w, r, appID, jobID)
	case action == "dataset" && r.Method == http.MethodGet:
		dataset, err := jobs.Dataset(ctx, appID, jobID)
		writeWorkJobResult(w, http.StatusOK, dataset, err)
	case action == "credentials" && r.Method == http.MethodGet:
		result, err := jobs.CredentialState(ctx, appID, jobID)
		writeWorkJobResult(w, http.StatusOK, result, err)
	case action == "credentials" && r.Method == http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, maxWorkJobSmallBody)
		var req sdk.UpdateWorkJobCredentialsRequest
		if err := decodeStrictJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		result, err := jobs.UpdateCredentials(ctx, appID, jobID, req)
		writeWorkJobResult(w, http.StatusOK, result, err)
	case action == "revise" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxWorkJobSmallBody)
		var req sdk.ReviseWorkJobRequest
		if err := decodeStrictJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		job, err := jobs.Revise(ctx, appID, jobID, req)
		writeWorkJobResult(w, http.StatusOK, job, err)
	case action == "pause" && r.Method == http.MethodPost:
		job, err := jobs.Pause(ctx, appID, jobID)
		writeWorkJobResult(w, http.StatusOK, job, err)
	case action == "resume" && r.Method == http.MethodPost:
		job, err := jobs.Resume(ctx, appID, jobID)
		writeWorkJobResult(w, http.StatusOK, job, err)
	case action == "cancel" && r.Method == http.MethodPost:
		job, err := jobs.Cancel(ctx, appID, jobID)
		writeWorkJobResult(w, http.StatusOK, job, err)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// streamWorkJobEvents is the job's own event stream: persisted job events
// after the client's cursor, then new ones as they are written. It never
// carries worker deltas and is separate from every run stream.
func (s *Server) streamWorkJobEvents(w http.ResponseWriter, r *http.Request, appID, jobID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	after, err := parseOptionalInt64(defaultString(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")))
	if err != nil || after < 0 {
		writeError(w, http.StatusBadRequest, "after must be a non-negative integer")
		return
	}
	if _, err := s.cfg.WorkJobs.Get(r.Context(), appID, jobID); err != nil {
		writeWorkJobError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	ticker := time.NewTicker(workJobStreamPoll)
	defer ticker.Stop()
	for {
		page, err := s.cfg.WorkJobs.Events(r.Context(), appID, jobID, after, 500)
		if err != nil {
			return
		}
		for _, event := range page.Items {
			raw, _ := json.Marshal(event)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Type, raw)
			after = event.Seq
		}
		if len(page.Items) > 0 {
			flusher.Flush()
		}
		if len(page.Items) == 500 {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func writeWorkJobResult(w http.ResponseWriter, status int, value interface{}, err error) {
	if err != nil {
		writeWorkJobError(w, err)
		return
	}
	writeJSON(w, status, value)
}

func writeWorkJobError(w http.ResponseWriter, err error) {
	var validation *workjobs.ValidationError
	switch {
	case errors.As(err, &validation):
		writeError(w, http.StatusBadRequest, validation.Message)
	case errors.Is(err, agentcore.ErrWorkJobNotFound), errors.Is(err, workjobs.ErrDisabled):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, workjobs.ErrUnsupported), errors.Is(err, agentcore.ErrWorkJobsUnsupported):
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": workjobs.ErrUnsupported.Error(), "reason": workjobs.ReasonUnsupportedStore})
	case errors.Is(err, agentcore.ErrWorkJobRevisionConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
