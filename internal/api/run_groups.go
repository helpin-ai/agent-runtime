package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	sdk "github.com/helpin-ai/agent-runtime-go"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
)

// runGroupsCapability reports run group availability for one app. It is nil
// without an app_id so clients never read another app's answer.
func (s *Server) runGroupsCapability(appID string) *sdk.FeatureCapability {
	appID = strings.TrimSpace(appID)
	if appID == "" || s.cfg.Engine == nil {
		return nil
	}
	supported, reason := s.cfg.Engine.RunGroupsAvailable(appID)
	return &sdk.FeatureCapability{Supported: supported, Reason: reason}
}

func (s *Server) createRunGroup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req sdk.CreateRunGroupRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.AppID = strings.TrimSpace(req.AppID)
	if req.AppID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	if !s.runGroupsAllowed(w, req.AppID) {
		return
	}
	if req.Policy.MaxActiveRuns < 0 {
		writeError(w, http.StatusBadRequest, "policy.max_active_runs must not be negative")
		return
	}
	group, err := s.cfg.Engine.CreateRunGroup(r.Context(), agentcore.RunGroup{
		AppID: req.AppID, CreationKey: req.CreationKey, ParentRunID: req.ParentRunID, Policy: req.Policy,
	})
	if err != nil {
		writeRunGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, group)
}

// runGroupSubroutes serves /v1/run-groups/{group_id}[/members|/completions|
// /pause|/resume|/cancel]. Apps without run groups receive 404, as if the
// routes did not exist.
func (s *Server) runGroupSubroutes(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/run-groups/"), "/"), "/")
	groupID, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(groupID) == "" || len(parts) > 2 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	if appID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	if !s.runGroupsAllowed(w, appID) {
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	ctx := r.Context()
	switch {
	case action == "" && r.Method == http.MethodGet:
		group, err := s.cfg.Engine.GetRunGroup(ctx, appID, groupID)
		writeRunGroupResult(w, group, err)
	case action == "members" && r.Method == http.MethodGet:
		members, err := s.cfg.Engine.ListRunGroupMembers(ctx, appID, groupID)
		if err == nil && members == nil {
			members = []agentcore.RunGroupMember{}
		}
		writeRunGroupResult(w, members, err)
	case action == "completions" && r.Method == http.MethodGet:
		after, afterErr := parseOptionalInt64(r.URL.Query().Get("after"))
		limit, limitErr := parseOptionalInt64(r.URL.Query().Get("limit"))
		if afterErr != nil || limitErr != nil || after < 0 || limit < 0 || limit > agentcore.MaxRunGroupCompletionLimit {
			writeError(w, http.StatusBadRequest, "after and limit must be non-negative integers; limit is at most 500")
			return
		}
		page, err := s.cfg.Engine.ListRunGroupCompletions(ctx, appID, groupID, after, int(limit))
		writeRunGroupResult(w, page, err)
	case action == "pause" && r.Method == http.MethodPost:
		group, err := s.cfg.Engine.PauseRunGroup(ctx, appID, groupID)
		writeRunGroupResult(w, group, err)
	case action == "resume" && r.Method == http.MethodPost:
		group, err := s.cfg.Engine.ResumeRunGroup(ctx, appID, groupID)
		writeRunGroupResult(w, group, err)
	case action == "cancel" && r.Method == http.MethodPost:
		group, err := s.cfg.Engine.CancelRunGroup(ctx, appID, groupID)
		writeRunGroupResult(w, group, err)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) runGroupsAllowed(w http.ResponseWriter, appID string) bool {
	if s.cfg.Engine == nil {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	if ok, _ := s.cfg.Engine.RunGroupsAvailable(appID); !ok {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
}

func writeRunGroupResult(w http.ResponseWriter, value interface{}, err error) {
	if err != nil {
		writeRunGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func writeRunGroupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentcore.ErrRunGroupNotFound),
		errors.Is(err, agentcore.ErrRunGroupsUnsupported),
		errors.Is(err, engine.ErrRunGroupsDisabled):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, agentcore.ErrRunGroupClosed), errors.Is(err, agentcore.ErrRunGroupFull):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func isRunGroupError(err error) bool {
	for _, target := range []error{agentcore.ErrRunGroupNotFound, agentcore.ErrRunGroupsUnsupported, engine.ErrRunGroupsDisabled, agentcore.ErrRunGroupClosed, agentcore.ErrRunGroupFull} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func parseOptionalInt64(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}
