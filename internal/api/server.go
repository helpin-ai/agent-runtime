package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type Config struct {
	Engine       *engine.Engine
	Store        agentcore.Store
	Tools        *tools.Registry
	CodexAuth    *runtime.CodexAuthManager
	ServiceToken string
	Capabilities Capabilities
	Events       *engine.EventBroker
}

type Server struct {
	cfg Config
	mux *http.ServeMux
}

func NewServer(cfg Config) http.Handler {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /internal/capabilities", s.withServiceAuth(s.capabilities))
	s.mux.HandleFunc("GET /v1/capabilities", s.withServiceAuth(s.capabilities))
	s.mux.HandleFunc("GET /internal/agents", s.withServiceAuth(s.listAgents))
	s.mux.HandleFunc("POST /internal/agents", s.withServiceAuth(s.createAgent))
	s.mux.HandleFunc("/internal/agents/", s.withServiceAuth(s.agentSubroutes))
	s.mux.HandleFunc("GET /internal/runs", s.withServiceAuth(s.listRuns))
	s.mux.HandleFunc("POST /internal/runs", s.withServiceAuth(s.startRun))
	s.mux.HandleFunc("/internal/runs/", s.withServiceAuth(s.runSubroutes))
	s.mux.HandleFunc("GET /v1/agents", s.withServiceAuth(s.listAgents))
	s.mux.HandleFunc("POST /v1/agents", s.withServiceAuth(s.createAgent))
	s.mux.HandleFunc("/v1/agents/", s.withServiceAuth(s.agentSubroutes))
	s.mux.HandleFunc("GET /v1/runs", s.withServiceAuth(s.listRuns))
	s.mux.HandleFunc("POST /v1/runs", s.withServiceAuth(s.startRun))
	s.mux.HandleFunc("/v1/runs/", s.withServiceAuth(s.runSubroutes))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var agent agentcore.Agent
	if err := decodeJSON(r, &agent); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.cfg.Store.CreateAgent(r.Context(), &agent); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, agent)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	if appID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	agents, err := s.cfg.Store.ListAgents(r.Context(), appID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) agentSubroutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/internal/agents/")
	path = strings.TrimPrefix(path, "/v1/agents/")
	agentID := strings.Trim(path, "/")
	if agentID == "" || strings.Contains(agentID, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getAgent(w, r, agentID)
	case http.MethodPut:
		s.upsertAgent(w, r, agentID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, agentID string) {
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	if appID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	agent, err := s.cfg.Store.GetAgent(r.Context(), appID, agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agent == nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) upsertAgent(w http.ResponseWriter, r *http.Request, agentID string) {
	var agent agentcore.Agent
	if err := decodeJSON(r, &agent); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	if appID == "" {
		appID = strings.TrimSpace(agent.AppID)
	}
	if appID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	agent.AppID = appID
	agent.ID = agentID
	existing, err := s.cfg.Store.GetAgent(r.Context(), appID, agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		if err := s.cfg.Store.CreateAgent(r.Context(), &agent); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, agent)
		return
	}
	if err := s.cfg.Store.UpdateAgent(r.Context(), &agent); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var req engine.StartRunRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, err := s.cfg.Engine.StartRun(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	if appID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	runs, err := s.cfg.Store.ListRuns(r.Context(), appID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) runSubroutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/internal/runs/")
	path = strings.TrimPrefix(path, "/v1/runs/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	runID := parts[0]
	appID := strings.TrimSpace(r.URL.Query().Get("app_id"))
	if appID == "" {
		writeError(w, http.StatusBadRequest, "app_id is required")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		s.getRun(w, r, appID, runID)
		return
	}
	if len(parts) == 4 && parts[1] == "codex-auth" && parts[2] == "device-code" {
		switch parts[3] {
		case "start":
			if r.Method == http.MethodPost {
				s.startCodexDeviceCodeAuth(w, r, appID, runID)
				return
			}
		case "cancel":
			if r.Method == http.MethodPost {
				s.cancelCodexDeviceCodeAuth(w, r, appID, runID)
				return
			}
		}
	}
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch parts[1] {
	case "events":
		if r.Method == http.MethodGet {
			s.runEvents(w, r, runID)
			return
		}
	case "messages":
		if r.Method == http.MethodGet {
			s.listMessages(w, r, appID, runID)
			return
		}
		if r.Method == http.MethodPost {
			s.appendMessage(w, r, appID, runID)
			return
		}
	case "artifacts":
		if r.Method == http.MethodGet {
			s.listArtifacts(w, r, appID, runID)
			return
		}
		if r.Method == http.MethodPost {
			s.appendArtifact(w, r, appID, runID)
			return
		}
	case "interactions":
		if r.Method == http.MethodGet {
			s.listInteractions(w, r, appID, runID)
			return
		}
	case "tool-calls":
		if r.Method == http.MethodGet {
			s.listToolCalls(w, r, appID, runID)
			return
		}
	case "tools":
		if !s.authorizeInternal(w, r) {
			return
		}
		if r.Method == http.MethodGet {
			s.listRunTools(w, r, appID, runID)
			return
		}
		if r.Method == http.MethodPost {
			s.callRunTool(w, r, appID, runID)
			return
		}
	case "resume":
		if r.Method == http.MethodPost {
			s.resumeRun(w, r, appID, runID)
			return
		}
	case "approve":
		if r.Method == http.MethodPost {
			s.approveRun(w, r, appID, runID)
			return
		}
	case "request-changes":
		if r.Method == http.MethodPost {
			s.requestChanges(w, r, appID, runID)
			return
		}
	case "cancel":
		if r.Method == http.MethodPost {
			s.cancelRun(w, r, appID, runID)
			return
		}
	}
	writeError(w, http.StatusNotFound, "not found")
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request, appID, runID string) {
	run, err := s.cfg.Store.GetRun(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, appID, runID string) {
	items, err := s.cfg.Store.ListMessages(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) appendMessage(w http.ResponseWriter, r *http.Request, appID, runID string) {
	var req struct {
		Content string `json:"content"`
		Role    string `json:"role"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "user"
	}
	msg := &agentcore.AgentRunMessage{
		AppID:       appID,
		RunID:       runID,
		Role:        role,
		Content:     strings.TrimSpace(req.Content),
		MessageType: "message",
	}
	if err := s.cfg.Store.AppendMessage(r.Context(), msg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request, appID, runID string) {
	items, err := s.cfg.Store.ListArtifacts(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) appendArtifact(w http.ResponseWriter, r *http.Request, appID, runID string) {
	var req struct {
		ArtifactType  string          `json:"artifact_type"`
		Format        string          `json:"format"`
		StorageMode   string          `json:"storage_mode"`
		InlineContent string          `json:"inline_content"`
		Metadata      json.RawMessage `json:"metadata"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	artifactType := strings.TrimSpace(req.ArtifactType)
	if artifactType == "" {
		writeError(w, http.StatusBadRequest, "artifact_type is required")
		return
	}
	format := strings.TrimSpace(req.Format)
	if format == "" {
		format = "json"
	}
	storageMode := strings.TrimSpace(req.StorageMode)
	if storageMode == "" {
		storageMode = "inline"
	}
	metadata := req.Metadata
	if len(metadata) == 0 || string(metadata) == "null" {
		metadata = json.RawMessage(`{}`)
	}
	artifact := &agentcore.AgentRunArtifact{
		AppID:         appID,
		RunID:         runID,
		ArtifactType:  artifactType,
		Format:        format,
		StorageMode:   storageMode,
		InlineContent: req.InlineContent,
		Metadata:      metadata,
	}
	if err := s.cfg.Store.AppendArtifact(r.Context(), artifact); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, artifact)
}

func (s *Server) listInteractions(w http.ResponseWriter, r *http.Request, appID, runID string) {
	items, err := s.cfg.Store.ListInteractions(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listToolCalls(w http.ResponseWriter, r *http.Request, appID, runID string) {
	items, err := s.cfg.Store.ListToolCalls(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listRunTools(w http.ResponseWriter, r *http.Request, appID, runID string) {
	gateway := mcp.NewGateway(s.cfg.Store, s.cfg.Tools)
	items, err := gateway.ListTools(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"tools": items})
}

func (s *Server) callRunTool(w http.ResponseWriter, r *http.Request, appID, runID string) {
	var req mcp.ToolCallRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	gateway := mcp.NewGateway(s.cfg.Store, s.cfg.Tools)
	result, err := gateway.CallTool(r.Context(), appID, runID, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request, appID, runID string) {
	var payload engine.ResumePayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, err := s.cfg.Engine.ResumeRun(r.Context(), appID, runID, payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) approveRun(w http.ResponseWriter, r *http.Request, appID, runID string) {
	run, err := s.cfg.Engine.ResumeRun(r.Context(), appID, runID, engine.ResumePayload{Intent: "approve"})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) requestChanges(w http.ResponseWriter, r *http.Request, appID, runID string) {
	var req struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, err := s.cfg.Engine.ResumeRun(r.Context(), appID, runID, engine.ResumePayload{
		Intent:  "request_changes",
		Content: req.Content,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request, appID, runID string) {
	run, err := s.cfg.Engine.CancelRun(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) startCodexDeviceCodeAuth(w http.ResponseWriter, r *http.Request, appID, runID string) {
	if s.cfg.CodexAuth == nil {
		writeError(w, http.StatusBadRequest, "codex auth manager is not configured")
		return
	}
	state, err := s.cfg.CodexAuth.StartDeviceCode(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) cancelCodexDeviceCodeAuth(w http.ResponseWriter, r *http.Request, appID, runID string) {
	if s.cfg.CodexAuth == nil {
		writeError(w, http.StatusBadRequest, "codex auth manager is not configured")
		return
	}
	state, err := s.cfg.CodexAuth.CancelDeviceCode(r.Context(), appID, runID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func decodeJSON(r *http.Request, out interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(out)
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) authorizeInternal(w http.ResponseWriter, r *http.Request) bool {
	token := strings.TrimSpace(s.cfg.ServiceToken)
	if token == "" {
		return true
	}
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(got), "bearer ") {
		got = strings.TrimSpace(got[len("Bearer "):])
	}
	if got != token {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (s *Server) withServiceAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorizeInternal(w, r) {
			return
		}
		next(w, r)
	}
}
