package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/completion"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/id"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
	"go.temporal.io/api/serviceerror"
)

const (
	ExecutionModeDurable     = "durable"
	ExecutionModeLightweight = "lightweight"
)

type Config struct {
	// ManualLightweightExecution lets an embedded caller own execution and cancellation.
	// The caller must invoke ExecuteRunOnce after StartRun or ResumeRun.
	ManualLightweightExecution bool

	// RequireRunModelCredentials is trusted deployment policy, never run input.
	RequireRunModelCredentials func(appID string) bool
	ValidateRunModelEndpoint   func(appID string, model *sdk.RunModel) error
	ModelCredentials           *modelauth.Manager
	DefaultExecutionMode       string
	Store                      agentcore.Store
	Runtimes                   *runtime.Registry
	Tools                      *tools.Registry
	Targets                    host.TargetContextProvider
	Skills                     *skills.Registry
	SkillPackages              *skills.PackageStoreRegistry
	Workspaces                 *workspace.Registry
	Durable                    DurableExecutor
	EventSink                  EventSink
	RunMCP                     mcp.RunConfig
	CodingWorker               bool
	CheckCodingAdmission       func(context.Context) error
}

type Engine struct {
	cfg Config
}

type DurableExecutor interface {
	StartRun(ctx context.Context, run *agentcore.AgentRun) error
	CancelRun(ctx context.Context, run *agentcore.AgentRun) error
	ResumeRun(ctx context.Context, run *agentcore.AgentRun, payload ResumePayload) error
}

const (
	DurableExecutionRunning   = "running"
	DurableExecutionCompleted = "completed"
	DurableExecutionFailed    = "failed"
	DurableExecutionCancelled = "cancelled"
	DurableExecutionMissing   = "missing"
)

type DurableExecutionInspector interface {
	InspectRun(ctx context.Context, run *agentcore.AgentRun) (string, error)
}

type RunExecutionInfo struct {
	ExecutionMode        string     `json:"execution_mode"`
	State                string     `json:"state"`
	WorkflowID           string     `json:"workflow_id,omitempty"`
	TemporalRunID        string     `json:"temporal_run_id,omitempty"`
	TaskQueue            string     `json:"task_queue,omitempty"`
	HistoryLength        int64      `json:"history_length,omitempty"`
	HistorySizeBytes     int64      `json:"history_size_bytes,omitempty"`
	StateTransitionCount int64      `json:"state_transition_count,omitempty"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	ClosedAt             *time.Time `json:"closed_at,omitempty"`
}

type DurableExecutionDescriber interface {
	DescribeRun(ctx context.Context, run *agentcore.AgentRun) (*RunExecutionInfo, error)
}

type EventSink interface {
	Emit(ctx context.Context, event Event)
}

type Event = sdk.Event

type SlogEventSink struct{}

func (SlogEventSink) Emit(_ context.Context, event Event) {
	slog.Info("agent runtime event", "app_id", event.AppID, "run_id", event.RunID, "type", event.Type)
}

type StartRunRequest struct {
	Model           *sdk.RunModel          `json:"model,omitempty"`
	ModelCredential *sdk.ModelCredential   `json:"model_credential,omitempty"`
	AppID           string                 `json:"app_id"`
	HostRunID       string                 `json:"host_run_id,omitempty"`
	AgentID         string                 `json:"agent_id"`
	Target          agentcore.TargetRef    `json:"target"`
	Instructions    string                 `json:"instructions,omitempty"`
	AllowedTools    []string               `json:"allowed_tools,omitempty"`
	ExternalActorID string                 `json:"external_actor_id,omitempty"`
	Mode            string                 `json:"mode,omitempty"`
	ExecutionMode   string                 `json:"execution_mode,omitempty"`
	Trigger         map[string]interface{} `json:"trigger,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
	TurnPolicy      agentcore.TurnPolicy   `json:"turn_policy,omitempty"`
	MCPServers      []mcp.RunServerRequest `json:"mcp_servers,omitempty"`
}

// RunMCPCredentialUpdate is the non-secret acknowledgement returned after a
// host app rotates one existing run-scoped MCP credential.
type RunMCPCredentialUpdate struct {
	RunID     string     `json:"run_id"`
	ServerID  string     `json:"server_id"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ResumePayload extends the public SDK request with optional correlation
// fields. Older clients can omit both fields; newer hosts should send a stable
// resume_id for retries and the interaction_id they are resolving.
type ResumePayload struct {
	MessageProvenance string                `json:"message_provenance,omitempty"`
	Intent            string                `json:"intent"`
	Content           string                `json:"content,omitempty"`
	ResponsePayload   json.RawMessage       `json:"response_payload,omitempty"`
	ExternalActorID   string                `json:"external_actor_id,omitempty"`
	ResumeID          string                `json:"resume_id,omitempty"`
	InteractionID     string                `json:"interaction_id,omitempty"`
	TurnPolicy        *agentcore.TurnPolicy `json:"turn_policy,omitempty"`
}

func New(cfg Config) *Engine {
	if cfg.DefaultExecutionMode == "" {
		cfg.DefaultExecutionMode = ExecutionModeLightweight
	}
	return &Engine{cfg: cfg}
}

// ExecutesCommandCapableRuns reports whether this engine runs in an execution
// role, that is one that owns a writable workspace volume and serves the
// command-capable queues. Such roles fence each run's workspace with a lock
// before touching it.
func (e *Engine) ExecutesCommandCapableRuns() bool {
	return e != nil && e.cfg.CodingWorker
}

func (e *Engine) StartRun(ctx context.Context, req StartRunRequest) (*agentcore.AgentRun, error) {
	if e == nil || e.cfg.Store == nil {
		return nil, fmt.Errorf("engine store is not configured")
	}
	req.AppID = strings.TrimSpace(req.AppID)
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.HostRunID = strings.TrimSpace(req.HostRunID)
	req.Target.Type = strings.TrimSpace(req.Target.Type)
	req.Target.ID = strings.TrimSpace(req.Target.ID)
	if req.AppID == "" || req.AgentID == "" {
		return nil, fmt.Errorf("app_id and agent_id are required")
	}
	if req.Target.Type == "" || req.Target.ID == "" {
		return nil, fmt.Errorf("target.type and target.id are required")
	}
	if req.HostRunID != "" {
		existing, err := e.cfg.Store.GetRunByHostRunID(ctx, req.AppID, req.HostRunID)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if !agentcore.IsTerminalStatus(existing.Status) {
				if err := e.admitExistingRun(ctx, existing); err != nil {
					return nil, err
				}
			}
			// A previous request may have committed the queued row and then
			// failed to start Temporal. Retrying the same host_run_id repairs
			// that split-brain instead of returning a permanently queued run.
			if existing.ExecutionMode == ExecutionModeDurable && existing.Status == agentcore.RunStatusQueued && e.cfg.Durable != nil {
				if err := e.cfg.Durable.StartRun(ctx, existing); err != nil {
					return nil, err
				}
			}
			return existing, nil
		}
	}
	agent, err := e.cfg.Store.GetAgent(ctx, req.AppID, req.AgentID)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, fmt.Errorf("agent not found")
	}
	if e.requiresRunModelCredentials(req.AppID) && (req.Model == nil || req.ModelCredential == nil) {
		return nil, ErrRunModelCredentialsRequired
	}
	if err := modelauth.ValidateModel(req.Model); err != nil {
		return nil, err
	}
	if err := e.admitModelEndpoint(req.AppID, req.Model); err != nil {
		return nil, err
	}
	if req.Model != nil && req.Model.Provider == "openai_compatible" && (req.ModelCredential == nil || req.ModelCredential.Type != req.Model.Endpoint.AuthMode) {
		return nil, errors.New("compatible endpoint requires its explicit credential mode")
	}
	if req.Model != nil && req.Model.Provider == "openai_chatgpt" && req.ModelCredential == nil {
		return nil, fmt.Errorf("ChatGPT requires a run credential")
	}
	if err := agentcore.ValidateTurnPolicy(req.TurnPolicy, agent.RuntimeKind); err != nil {
		return nil, fmt.Errorf("invalid turn_policy: %w", err)
	}
	if len(agent.AllowedTargets) > 0 && !slices.Contains(agent.AllowedTargets, req.Target.Type) {
		return nil, fmt.Errorf("target type %q is not allowed for agent", req.Target.Type)
	}
	if err := tools.ValidateAllowedSubset(agent, req.AllowedTools); err != nil {
		return nil, err
	}
	admissionMode := strings.TrimSpace(req.ExecutionMode)
	if admissionMode == "" {
		admissionMode = e.cfg.DefaultExecutionMode
	}
	if err := e.admitTools(ctx, agent, req.AllowedTools, admissionMode); err != nil {
		return nil, err
	}
	metadata := make(map[string]interface{}, len(req.Metadata)+1)
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	metadata[CodingMetadataKey] = tools.RequiresCoding(tools.AllowedSet(agent, req.AllowedTools))
	req.Metadata = metadata

	runID := id.New("run")
	targetContext, err := e.resolveTargetForRun(ctx, host.TargetContextRequest{
		AppID:    req.AppID,
		RunID:    runID,
		AgentID:  agent.ID,
		Target:   req.Target,
		Trigger:  req.Trigger,
		Metadata: req.Metadata,
	})
	if err != nil {
		return nil, err
	}
	mode := strings.TrimSpace(req.ExecutionMode)
	if mode == "" {
		mode = e.cfg.DefaultExecutionMode
	}
	switch mode {
	case ExecutionModeLightweight:
	case ExecutionModeDurable:
		if e.cfg.Durable == nil {
			return nil, fmt.Errorf("durable execution requested but durable executor is not configured")
		}
	default:
		return nil, fmt.Errorf("unsupported execution_mode %q", mode)
	}
	invocation := strings.TrimSpace(req.Mode)
	if invocation == "" {
		invocation = agent.DefaultInvocationMode
	}
	if invocation == "" {
		invocation = agentcore.InvocationAutonomous
	}
	run := &agentcore.AgentRun{
		ID:              runID,
		AppID:           req.AppID,
		HostRunID:       req.HostRunID,
		AgentID:         agent.ID,
		Target:          req.Target,
		RuntimeKind:     agent.RuntimeKind,
		ExecutionMode:   mode,
		InvocationMode:  invocation,
		ExternalActorID: strings.TrimSpace(req.ExternalActorID),
		Status:          agentcore.RunStatusQueued,
		PauseReason:     agentcore.PauseReasonNone,
		ApprovalState:   agentcore.InitialApprovalState(agent),
		Input: agentcore.RunInput{
			Model:          req.Model,
			Instructions:   strings.TrimSpace(req.Instructions),
			AllowedTools:   normalizeTools(req.AllowedTools),
			Trigger:        req.Trigger,
			Metadata:       req.Metadata,
			ContextSummary: targetContext.Summary,
			TurnPolicy:     agentcore.NormalizeTurnPolicy(req.TurnPolicy),
		},
		OutputSummary: json.RawMessage(`{}`),
	}
	runMCPServers, err := mcp.PrepareStoredServers(run.AppID, run.ID, req.MCPServers, e.cfg.RunMCP)
	if err != nil {
		return nil, err
	}
	var createErr error
	if req.ModelCredential != nil {
		provider := agent.Provider
		if req.Model != nil {
			provider = req.Model.Provider
		}
		credential, err := e.cfg.ModelCredentials.Prepare(run.AppID, run.ID, provider, *req.ModelCredential)
		if err != nil {
			return nil, err
		}
		if run.Input.Model == nil {
			run.Input.Model = &sdk.RunModel{Provider: provider, Model: agent.Model}
		}
		run.Input.CredentialSource = "app"
		createErr = e.cfg.ModelCredentials.Store.CreateRunWithModelCredential(ctx, run, runMCPServers, credential)
	} else {
		createErr = e.cfg.Store.CreateRunWithMCP(ctx, run, runMCPServers)
	}
	if err := createErr; err != nil {
		if req.HostRunID != "" {
			existing, lookupErr := e.cfg.Store.GetRunByHostRunID(ctx, req.AppID, req.HostRunID)
			if lookupErr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}
	e.emitRunEvent(ctx, run, "run.queued", nil)

	switch mode {
	case ExecutionModeLightweight:
		if !e.cfg.ManualLightweightExecution {
			go e.executeLightweight(context.Background(), run.AppID, run.ID)
		}
	case ExecutionModeDurable:
		if err := e.cfg.Durable.StartRun(ctx, run); err != nil {
			e.failRun(ctx, run, err.Error())
			return nil, err
		}
	}
	return run, nil
}

// UpdateRunMCPCredential replaces only the encrypted credential for an
// existing run attachment. Server identity, URL, transport, and tool policy
// remain immutable for the lifetime of the run.
func (e *Engine) UpdateRunMCPCredential(
	ctx context.Context,
	appID, runID, serverID string,
	credential mcp.RunCredential,
) (*RunMCPCredentialUpdate, error) {
	run, err := e.requireRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if agentcore.IsTerminalStatus(run.Status) {
		return nil, fmt.Errorf("agent run is terminal")
	}
	serverID = strings.TrimSpace(serverID)
	servers, err := e.cfg.Store.ListRunMCPServers(ctx, run.AppID, run.ID)
	if err != nil {
		return nil, err
	}
	found := false
	for _, server := range servers {
		if server.ServerID == serverID {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("run MCP server not found")
	}
	encrypted, err := mcp.PrepareRotatedCredential(
		run.AppID, run.ID, serverID, credential, e.cfg.RunMCP,
	)
	if err != nil {
		return nil, err
	}
	if err := e.cfg.Store.UpdateRunMCPCredential(
		ctx, run.AppID, run.ID, serverID, encrypted,
	); err != nil {
		return nil, err
	}
	updatedAt := time.Now().UTC()
	e.emitRunEvent(ctx, run, "run.mcp_credential_updated", map[string]interface{}{
		"server_id":  serverID,
		"expires_at": credential.ExpiresAt,
	})
	return &RunMCPCredentialUpdate{
		RunID: run.ID, ServerID: serverID,
		ExpiresAt: credential.ExpiresAt, UpdatedAt: updatedAt,
	}, nil
}

// ReconcileDurableRuns repairs the narrow failure window where a run row was
// committed but its Temporal workflow was not started. When the durable
// executor supports inspection, it also terminates stale active database rows
// whose Temporal workflow is missing or already closed.
func (e *Engine) ReconcileDurableRuns(ctx context.Context, olderThan time.Time) (int, error) {
	if e == nil || e.cfg.Store == nil || e.cfg.Durable == nil {
		return 0, nil
	}
	runs, err := e.cfg.Store.ListRunsByStatus(ctx, agentcore.RunStatusQueued, agentcore.RunStatusRunning, agentcore.RunStatusPaused)
	if err != nil {
		return 0, err
	}
	reconciled := 0
	var reconcileErrs []error
	for i := range runs {
		run := &runs[i]
		if run.ExecutionMode != ExecutionModeDurable || (!olderThan.IsZero() && run.UpdatedAt.After(olderThan)) {
			continue
		}
		if run.Status == agentcore.RunStatusQueued {
			if err := e.admitStoredModelPolicy(run); err != nil {
				reconcileErrs = append(reconcileErrs, err)
				continue
			}
			if err := e.cfg.Durable.StartRun(ctx, run); err != nil {
				reconcileErrs = append(reconcileErrs, fmt.Errorf("start queued durable run %s/%s: %w", run.AppID, run.ID, err))
				continue
			}
			reconciled++
			continue
		}
		inspector, ok := e.cfg.Durable.(DurableExecutionInspector)
		if !ok {
			continue
		}
		state, err := inspector.InspectRun(ctx, run)
		if err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("inspect durable run %s/%s: %w", run.AppID, run.ID, err))
			continue
		}
		if state == DurableExecutionRunning {
			continue
		}
		message := fmt.Sprintf("temporal workflow is %s while runtime run remained %s", state, run.Status)
		e.failRun(ctx, run, message)
		reconciled++
	}
	return reconciled, errors.Join(reconcileErrs...)
}

func (e *Engine) GetRunExecution(ctx context.Context, appID, runID string) (*RunExecutionInfo, error) {
	run, err := e.requireRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	info := &RunExecutionInfo{ExecutionMode: run.ExecutionMode, State: run.Status}
	if run.ExecutionMode != ExecutionModeDurable || e.cfg.Durable == nil {
		return info, nil
	}
	if describer, ok := e.cfg.Durable.(DurableExecutionDescriber); ok {
		return describer.DescribeRun(ctx, run)
	}
	return info, nil
}

// RunToolGateway prepares the same isolated registry and allowlist used by an
// adapter execution. Callers must invoke the returned close function.
func (e *Engine) RunToolGateway(ctx context.Context, appID, runID string) (*mcp.Gateway, func(), error) {
	if e == nil || e.cfg.Store == nil {
		return nil, func() {}, fmt.Errorf("engine store is not configured")
	}
	run, err := e.cfg.Store.GetRun(ctx, strings.TrimSpace(appID), strings.TrimSpace(runID))
	if err != nil || run == nil {
		if err == nil {
			err = fmt.Errorf("agent run not found")
		}
		return nil, func() {}, err
	}
	if agentcore.IsTerminalStatus(run.Status) {
		return nil, func() {}, fmt.Errorf("agent run is not active")
	}
	agent, err := e.cfg.Store.GetAgent(ctx, run.AppID, run.AgentID)
	if err != nil || agent == nil {
		if err == nil {
			err = fmt.Errorf("agent not found")
		}
		return nil, func() {}, err
	}
	registry, runAllowed, _, closeRunMCP, err := mcp.PrepareRunTools(ctx, e.cfg.Store, e.cfg.Tools, run.AppID, run.ID, e.cfg.RunMCP)
	if err != nil {
		return nil, func() {}, err
	}
	allowed := tools.AllowedSet(agent, run.Input.AllowedTools)
	for name := range runAllowed {
		allowed[name] = true
	}
	return mcp.NewGatewayWithAllowed(e.cfg.Store, registry, allowed), closeRunMCP, nil
}

func (e *Engine) CancelRun(ctx context.Context, appID, runID string) (*agentcore.AgentRun, error) {
	run, err := e.requireRunOrHostRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if agentcore.IsTerminalStatus(run.Status) {
		if run.Status == agentcore.RunStatusCancelled {
			e.captureInterruptedEffectsBestEffort(ctx, run)
			e.cleanupWorkspace(ctx, run, "cancelled", true)
		}
		return run, nil
	}
	if run.ExecutionMode == ExecutionModeDurable && e.cfg.Durable != nil {
		if err := e.cfg.Durable.CancelRun(ctx, run); err != nil {
			return nil, err
		}
	}
	// Close admission before inspecting the checkpoint: tools persist their
	// marker and recheck this status before launch.
	now := time.Now().UTC()
	run.Status = agentcore.RunStatusCancelled
	run.PauseReason = agentcore.PauseReasonNone
	run.CompletedAt = &now
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		return nil, err
	}
	// The run is already terminal in the store. An unreadable checkpoint must
	// not leave credentials, tool resources, or the workspace behind, nor
	// suppress the cancellation event the host is waiting for.
	e.captureInterruptedEffectsBestEffort(ctx, run)
	e.cleanupWorkspace(ctx, run, "cancelled", true)
	e.clearRunCredentials(ctx, run)
	e.closeRunToolResources(ctx, run)
	e.emitRunEvent(ctx, run, "run.cancelled", e.terminalEventData(run, nil))
	return run, nil
}

// captureInterruptedEffectsBestEffort records interrupted tool outcomes when
// the checkpoint is readable and logs otherwise. Cancellation teardown never
// depends on it succeeding.
func (e *Engine) captureInterruptedEffectsBestEffort(ctx context.Context, run *agentcore.AgentRun) {
	if err := e.captureInterruptedEffects(ctx, run); err != nil {
		slog.WarnContext(ctx, "capture interrupted effects failed", "app_id", run.AppID, "run_id", run.ID, "error", err)
	}
}

func (e *Engine) captureInterruptedEffects(ctx context.Context, run *agentcore.AgentRun) error {
	effects, err := runtime.NativeInterruptedEffects(ctx, e.cfg.Store, run.AppID, run.ID)
	if err != nil {
		return fmt.Errorf("read interrupted operation state: %w", err)
	}
	if len(effects) > 0 {
		run.OutputSummary = mergeOutputSummaries(run.OutputSummary, effects)
		return e.cfg.Store.UpdateRun(ctx, run)
	}
	return nil
}

// requireRunOrHostRun resolves a runtime-owned run ID first, then the
// application-owned host_run_id. The fallback lets a host recover control of
// a run when the runtime accepted StartRun but the host failed to persist the
// returned runtime ID.
func (e *Engine) requireRunOrHostRun(ctx context.Context, appID, runID string) (*agentcore.AgentRun, error) {
	if e == nil || e.cfg.Store == nil {
		return nil, fmt.Errorf("engine store is not configured")
	}
	appID = strings.TrimSpace(appID)
	runID = strings.TrimSpace(runID)
	run, err := e.cfg.Store.GetRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if run != nil {
		return run, nil
	}
	run, err = e.cfg.Store.GetRunByHostRunID(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("run not found")
	}
	return run, nil
}

func (e *Engine) ResumeRun(ctx context.Context, appID, runID string, payload ResumePayload) (*agentcore.AgentRun, error) {
	run, err := e.requireRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if err := e.admitExistingRun(ctx, run); err != nil {
		return nil, err
	}
	payload.Intent = strings.TrimSpace(payload.Intent)
	payload.ResumeID = strings.TrimSpace(payload.ResumeID)
	payload.InteractionID = strings.TrimSpace(payload.InteractionID)
	payload.MessageProvenance = strings.TrimSpace(payload.MessageProvenance)
	switch payload.MessageProvenance {
	case "", "human", "system_notification":
	default:
		return nil, fmt.Errorf("message_provenance must be human or system_notification")
	}
	if payload.MessageProvenance == "human" && strings.TrimSpace(payload.ExternalActorID) == "" {
		return nil, fmt.Errorf("human messages require external_actor_id")
	}
	if payload.MessageProvenance == "system_notification" && payload.Intent != "reply" {
		return nil, fmt.Errorf("system notifications require reply intent")
	}
	if payload.MessageProvenance == "system_notification" && (payload.InteractionID != "" || len(payload.ResponsePayload) > 0) {
		return nil, fmt.Errorf("system notifications cannot include an interaction response")
	}
	if payload.MessageProvenance == "human" && payload.Intent == "auth_completed" {
		return nil, fmt.Errorf("authentication completion is a host event")
	}
	if payload.TurnPolicy != nil {
		if err := agentcore.ValidateTurnPolicy(*payload.TurnPolicy, run.RuntimeKind); err != nil {
			return nil, fmt.Errorf("invalid turn_policy: %w", err)
		}
	}
	fingerprint := resumeFingerprint(payload)
	if previousResumeMatches(run, payload.ResumeID, fingerprint) {
		return run, nil
	}
	if agentcore.IsTerminalStatus(run.Status) {
		return nil, fmt.Errorf("run is terminal")
	}
	if run.Status != agentcore.RunStatusPaused {
		return nil, fmt.Errorf("run is not paused")
	}
	if payload.MessageProvenance == "system_notification" && run.PauseReason != agentcore.PauseReasonUserMessage {
		return nil, fmt.Errorf("system notifications cannot resolve pending interactions")
	}
	if e.chatRunIdleExpired(run) {
		if err := e.completeIdleChatRun(ctx, run); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("run idle timeout expired")
	}
	originalRun := cloneRunForRollback(run)
	if payload.TurnPolicy != nil {
		run.Input.TurnPolicy = agentcore.NormalizeTurnPolicy(*payload.TurnPolicy)
	}
	interaction, resolved, err := e.resolvePendingInteraction(ctx, run, payload)
	if err != nil {
		return nil, err
	}
	if payload.InteractionID != "" && !resolved {
		if interaction != nil && strings.TrimSpace(interaction.Status) == "resolved" && interactionResponseMatches(interaction, payload) {
			return run, nil
		}
		return nil, fmt.Errorf("interaction %q is not pending", payload.InteractionID)
	}
	if payload.InteractionID == "" && interaction != nil {
		payload.InteractionID = interaction.ID
	}
	if payload.ResumeID == "" {
		if interaction != nil {
			payload.ResumeID = "interaction:" + interaction.ID + ":" + payload.Intent
		} else {
			// Older SDKs do not send resume_id. Derive one from the paused
			// row version so concurrent retries of this turn converge on the
			// same Temporal signal ID while a later paused turn gets a new ID.
			payload.ResumeID = fmt.Sprintf("auto:%s:%d:%s", run.ID, run.UpdatedAt.UnixNano(), fingerprint[:16])
		}
	}
	if strings.TrimSpace(payload.Content) != "" && !e.resumeMessageExists(ctx, run, payload.ResumeID) {
		messageType := "message"
		if payload.MessageProvenance == "system_notification" {
			messageType = "system_notification"
		}
		if err := e.cfg.Store.AppendMessage(ctx, &agentcore.AgentRunMessage{
			AppID:            run.AppID,
			RunID:            run.ID,
			RuntimeMessageID: payload.ResumeID,
			Role:             "user",
			Content:          strings.TrimSpace(payload.Content),
			MessageType:      messageType,
		}); err != nil {
			e.rollbackResolvedInteraction(ctx, interaction, resolved)
			return nil, err
		}
	}
	if run.Input.Metadata == nil {
		run.Input.Metadata = map[string]interface{}{}
	}
	lastResume := map[string]interface{}{
		"message_provenance": payload.MessageProvenance,
		"intent":             strings.TrimSpace(payload.Intent),
		"content":            strings.TrimSpace(payload.Content),
		"external_actor_id":  strings.TrimSpace(payload.ExternalActorID),
		"resume_id":          payload.ResumeID,
		"interaction_id":     payload.InteractionID,
		"fingerprint":        fingerprint,
	}
	if len(payload.ResponsePayload) > 0 {
		lastResume["response_payload"] = json.RawMessage(append(json.RawMessage(nil), payload.ResponsePayload...))
	}
	run.Input.Metadata["last_resume"] = lastResume
	run.Input.Metadata["turn_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	run.Status = agentcore.RunStatusRunning
	run.PauseReason = agentcore.PauseReasonNone
	if payload.Intent == "approve" {
		run.ApprovalState = agentcore.ApprovalApproved
	}
	if payload.Intent == "request_changes" {
		run.ApprovalState = agentcore.ApprovalRejected
	}
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		e.rollbackResolvedInteraction(ctx, interaction, resolved)
		return nil, err
	}
	if run.ExecutionMode == ExecutionModeDurable && e.cfg.Durable != nil {
		if err := e.cfg.Durable.ResumeRun(ctx, run, payload); err != nil {
			if rollbackErr := e.cfg.Store.UpdateRun(ctx, originalRun); rollbackErr != nil {
				return nil, fmt.Errorf("signal durable run: %w (rollback run state: %v)", err, rollbackErr)
			}
			e.rollbackResolvedInteraction(ctx, interaction, resolved)
			return nil, err
		}
	}
	e.emitRunEvent(ctx, run, "run.resumed", map[string]interface{}{"resume_id": payload.ResumeID, "interaction_id": payload.InteractionID})
	if run.ExecutionMode == ExecutionModeLightweight {
		if !e.cfg.ManualLightweightExecution {
			go e.executeLightweight(context.Background(), run.AppID, run.ID)
		}
	}
	return run, nil
}

func (e *Engine) resolvePendingInteraction(ctx context.Context, run *agentcore.AgentRun, payload ResumePayload) (*agentcore.AgentRunInteraction, bool, error) {
	if e == nil || e.cfg.Store == nil || run == nil {
		return nil, false, nil
	}
	interactions, err := e.cfg.Store.ListInteractions(ctx, run.AppID, run.ID)
	if err != nil {
		return nil, false, err
	}
	if payload.MessageProvenance == "system_notification" {
		for _, interaction := range interactions {
			if strings.TrimSpace(interaction.Status) == "pending" {
				return nil, false, fmt.Errorf("system notifications cannot resolve pending interactions")
			}
		}
		return nil, false, nil
	}
	if payload.InteractionID != "" {
		for i := range interactions {
			interaction := interactions[i]
			if interaction.ID != payload.InteractionID {
				continue
			}
			if strings.TrimSpace(interaction.Status) != "pending" {
				return &interaction, false, nil
			}
			if err := e.resolveInteraction(ctx, &interaction, payload); err != nil {
				return nil, false, err
			}
			return &interaction, true, nil
		}
		return nil, false, nil
	}
	for i := len(interactions) - 1; i >= 0; i-- {
		interaction := interactions[i]
		if strings.TrimSpace(interaction.Status) != "pending" {
			continue
		}
		if err := e.resolveInteraction(ctx, &interaction, payload); err != nil {
			return nil, false, err
		}
		payload.InteractionID = interaction.ID
		return &interaction, true, nil
	}
	return nil, false, nil
}

func (e *Engine) resolveInteraction(ctx context.Context, interaction *agentcore.AgentRunInteraction, payload ResumePayload) error {
	interaction.Status = "resolved"
	interaction.ResolvedByExternalID = strings.TrimSpace(payload.ExternalActorID)
	resolvedAt := time.Now().UTC()
	interaction.ResolvedAt = &resolvedAt
	interaction.ResponsePayload = resumeInteractionResponsePayload(payload)
	return e.cfg.Store.UpdateInteraction(ctx, interaction)
}

func (e *Engine) rollbackResolvedInteraction(ctx context.Context, interaction *agentcore.AgentRunInteraction, resolved bool) {
	if !resolved || interaction == nil {
		return
	}
	interaction.Status = "pending"
	interaction.ResolvedByExternalID = ""
	interaction.ResolvedAt = nil
	interaction.ResponsePayload = nil
	_ = e.cfg.Store.UpdateInteraction(ctx, interaction)
}

func resumeInteractionResponsePayload(payload ResumePayload) json.RawMessage {
	if len(payload.ResponsePayload) > 0 {
		return append(json.RawMessage(nil), payload.ResponsePayload...)
	}
	body, _ := json.Marshal(map[string]interface{}{
		"intent":  strings.TrimSpace(payload.Intent),
		"content": strings.TrimSpace(payload.Content),
	})
	return body
}

func resumeFingerprint(payload ResumePayload) string {
	body, _ := json.Marshal(struct {
		MessageProvenance string                `json:"message_provenance,omitempty"`
		Intent            string                `json:"intent"`
		Content           string                `json:"content,omitempty"`
		ResponsePayload   json.RawMessage       `json:"response_payload,omitempty"`
		ExternalActorID   string                `json:"external_actor_id,omitempty"`
		InteractionID     string                `json:"interaction_id,omitempty"`
		TurnPolicy        *agentcore.TurnPolicy `json:"turn_policy,omitempty"`
	}{
		MessageProvenance: payload.MessageProvenance,
		Intent:            strings.TrimSpace(payload.Intent),
		Content:           strings.TrimSpace(payload.Content),
		ResponsePayload:   payload.ResponsePayload,
		ExternalActorID:   strings.TrimSpace(payload.ExternalActorID),
		InteractionID:     strings.TrimSpace(payload.InteractionID),
		TurnPolicy:        payload.TurnPolicy,
	})
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%x", sum[:])
}

func previousResumeMatches(run *agentcore.AgentRun, resumeID, fingerprint string) bool {
	if run == nil || run.Input.Metadata == nil {
		return false
	}
	last, ok := run.Input.Metadata["last_resume"].(map[string]interface{})
	if !ok {
		return false
	}
	previousID, _ := last["resume_id"].(string)
	if resumeID != "" && strings.TrimSpace(previousID) == strings.TrimSpace(resumeID) {
		return true
	}
	previousFingerprint, _ := last["fingerprint"].(string)
	return run.Status != agentcore.RunStatusPaused && fingerprint != "" && strings.TrimSpace(previousFingerprint) == fingerprint
}

func interactionResponseMatches(interaction *agentcore.AgentRunInteraction, payload ResumePayload) bool {
	if interaction == nil {
		return false
	}
	return strings.TrimSpace(string(interaction.ResponsePayload)) == strings.TrimSpace(string(resumeInteractionResponsePayload(payload)))
}

func cloneRunForRollback(run *agentcore.AgentRun) *agentcore.AgentRun {
	if run == nil {
		return nil
	}
	body, err := json.Marshal(run)
	if err == nil {
		var cloned agentcore.AgentRun
		if json.Unmarshal(body, &cloned) == nil {
			return &cloned
		}
	}
	cloned := *run
	cloned.Input.Metadata = copyStringAnyMap(run.Input.Metadata)
	return &cloned
}

func (e *Engine) resumeMessageExists(ctx context.Context, run *agentcore.AgentRun, resumeID string) bool {
	if e == nil || e.cfg.Store == nil || run == nil || strings.TrimSpace(resumeID) == "" {
		return false
	}
	messages, err := e.cfg.Store.ListMessages(ctx, run.AppID, run.ID)
	if err != nil {
		return false
	}
	for _, message := range messages {
		if strings.TrimSpace(message.RuntimeMessageID) == strings.TrimSpace(resumeID) {
			return true
		}
	}
	return false
}

func (e *Engine) chatRunIdleExpired(run *agentcore.AgentRun) bool {
	if run == nil || run.Status != agentcore.RunStatusPaused || run.PauseReason != agentcore.PauseReasonUserMessage {
		return false
	}
	policy := agentcore.NormalizeTurnPolicy(run.Input.TurnPolicy)
	if policy.Mode != agentcore.TurnPolicyPauseAfterAssist || policy.IdleTimeoutSeconds <= 0 {
		return false
	}
	return time.Since(run.UpdatedAt) > time.Duration(policy.IdleTimeoutSeconds)*time.Second
}

// completeIdleChatRun retires a paused chat run whose idle timeout elapsed.
// The durable execution is stopped before the run is marked completed, the
// same order Engine.CancelRun uses, so a Temporal failure leaves the run
// paused and retryable rather than completed with a live workflow behind it.
func (e *Engine) completeIdleChatRun(ctx context.Context, run *agentcore.AgentRun) error {
	if run.ExecutionMode == ExecutionModeDurable && e.cfg.Durable != nil {
		if err := e.cfg.Durable.CancelRun(ctx, run); err != nil && !durableExecutionAlreadyClosed(err) {
			return err
		}
	}
	now := time.Now().UTC()
	run.Status = agentcore.RunStatusCompleted
	run.PauseReason = agentcore.PauseReasonNone
	run.CompletedAt = &now
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		return err
	}
	e.clearRunCredentials(ctx, run)
	e.cleanupWorkspace(ctx, run, "completed", true)
	e.emitRunEvent(ctx, run, "run.completed", e.terminalEventData(run, map[string]interface{}{"reason": "idle_timeout"}))
	return nil
}

// durableExecutionAlreadyClosed reports a cancel request against a workflow
// that Temporal no longer tracks as open. DescribeRun maps the same NotFound
// to DurableExecutionMissing; for an idle chat run it means there is nothing
// left to stop.
func durableExecutionAlreadyClosed(err error) bool {
	if err == nil {
		return false
	}
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "already completed")
}

func (e *Engine) executeLightweight(ctx context.Context, appID, runID string) {
	if _, err := e.ExecuteRunOnce(ctx, appID, runID); err != nil {
		slog.Error("lightweight run execution failed", "app_id", appID, "run_id", runID, "error", err)
	}
}

func (e *Engine) resolveSkills(ctx context.Context, agent *agentcore.Agent, run *agentcore.AgentRun) (skills.Resolution, error) {
	if agent == nil {
		return skills.Resolution{}, nil
	}
	skillRefs, allowedTools, err := e.skillPolicyForRun(ctx, agent, run)
	if err != nil {
		return skills.Resolution{}, err
	}
	if len(skillRefs) == 0 {
		return skills.Resolution{}, nil
	}
	if e == nil || e.cfg.Skills == nil {
		return skills.Resolution{}, fmt.Errorf("skill registry is not configured")
	}
	lookupCtx := skills.LookupContext{AppID: agent.AppID}
	if run != nil {
		lookupCtx.AgentID = run.AgentID
		lookupCtx.RunID = run.ID
		lookupCtx.Target = run.Target
		lookupCtx.Trigger = run.Input.Trigger
		lookupCtx.Metadata = run.Input.Metadata
	}
	resolution, err := e.cfg.Skills.ResolveForContext(ctx, lookupCtx, skillRefs)
	if err != nil {
		return skills.Resolution{}, err
	}
	resolution = skills.SelectActiveResolution(resolution, activeSelectionContext(agent, run))
	if err := skills.ValidateRuntimeAndTools(agent.RuntimeKind, allowedTools, resolution.Definitions); err != nil {
		return skills.Resolution{}, err
	}
	if len(resolution.CoreRefs) > 0 {
		agent.Skills = resolution.CoreRefs
	}
	return resolution, nil
}

func (e *Engine) skillPolicyForRun(ctx context.Context, agent *agentcore.Agent, run *agentcore.AgentRun) ([]agentcore.SkillRef, []string, error) {
	if agent == nil {
		return nil, nil, nil
	}
	skillRefs := append([]agentcore.SkillRef(nil), agent.Skills...)
	allowedTools := append([]string(nil), agent.AllowedTools...)
	if run == nil {
		return skillRefs, allowedTools, nil
	}
	if len(run.Input.AllowedTools) > 0 {
		allowedTools = append([]string(nil), run.Input.AllowedTools...)
	}
	if e == nil || e.cfg.Store == nil {
		return nil, nil, fmt.Errorf("engine store is not configured")
	}
	servers, err := e.cfg.Store.ListRunMCPServers(ctx, run.AppID, run.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("list run MCP servers for skill resolution: %w", err)
	}
	for _, server := range servers {
		skillRefs = append(skillRefs, server.Skills...)
		for _, tool := range server.Tools {
			if alias := mcp.RunToolAlias(server.ServerName, tool.Name); alias != "" {
				allowedTools = append(allowedTools, alias)
			}
		}
	}
	return skillRefs, allowedTools, nil
}

func activeSelectionContext(agent *agentcore.Agent, run *agentcore.AgentRun) skills.ActiveSelectionContext {
	var ctx skills.ActiveSelectionContext
	if agent != nil {
		ctx.PresetKey = firstConfigString(agent.ExecutionConfig, "preset_key", "preset")
	}
	if run != nil {
		ctx.TargetType = strings.TrimSpace(run.Target.Type)
		ctx.PlanningStage = firstMapString(run.Input.Metadata, "planning_stage", "execution_stage", "stage")
		if ctx.PlanningStage == "" {
			ctx.PlanningStage = firstMapString(run.Input.Trigger, "planning_stage", "execution_stage", "stage")
		}
		if ctx.PresetKey == "" {
			ctx.PresetKey = firstMapString(run.Input.Metadata, "preset_key", "preset")
		}
		if ctx.PresetKey == "" {
			ctx.PresetKey = firstMapString(run.Input.Trigger, "preset_key", "preset")
		}
	}
	return ctx
}

func firstConfigString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var value map[string]interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return firstMapString(value, keys...)
}

func firstMapString(value map[string]interface{}, keys ...string) string {
	if len(value) == 0 {
		return ""
	}
	for _, key := range keys {
		raw, ok := value[key]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		case fmt.Stringer:
			if trimmed := strings.TrimSpace(typed.String()); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func (e *Engine) stageRuntimeSkills(ctx context.Context, agent *agentcore.Agent, run *agentcore.AgentRun, resolution skills.Resolution, lease *agentcore.WorkspaceLease, targetContext *host.TargetContext) (string, skills.Resolution, error) {
	if len(resolution.CoreRefs) == 0 || len(resolution.Definitions) == 0 {
		return e.stageRepositorySkillsOnly(run, resolution, lease, targetContext)
	}
	stagingResolution := resolution
	if resolution.UsesExplicitRoles {
		if len(resolution.AvailableRefs) == 0 {
			return e.stageRepositorySkillsOnly(run, resolution, lease, targetContext)
		}
		stagingResolution = skills.Resolution{
			CoreRefs:     append([]agentcore.SkillRef(nil), resolution.AvailableRefs...),
			Definitions:  append([]skills.Definition(nil), resolution.AvailableDefinitions...),
			Instructions: skills.CompileInstructions(resolution.AvailableDefinitions),
			Policy:       skills.AggregatePolicy(resolution.AvailableDefinitions),
		}
	}
	stageRoot := stagedSkillRootPath(run, lease)
	lookupCtx := skills.LookupContext{AppID: run.AppID}
	if run != nil {
		lookupCtx.AgentID = run.AgentID
		lookupCtx.RunID = run.ID
		lookupCtx.Target = run.Target
		lookupCtx.Trigger = run.Input.Trigger
		lookupCtx.Metadata = run.Input.Metadata
	}
	var packageStore skills.PackageStore
	if e != nil && e.cfg.SkillPackages != nil && run != nil {
		packageStore, _ = e.cfg.SkillPackages.Store(run.AppID)
	}
	var lookup skills.WorkspaceLookup
	if e != nil && e.cfg.Skills != nil && run != nil {
		lookup = e.cfg.Skills.WorkspaceLookupForApp(run.AppID)
	}
	if err := skills.StageResolvedInto(ctx, stagingResolution, skills.StageOptions{
		Lookup:        lookup,
		PackageStore:  packageStore,
		LookupContext: lookupCtx,
		DestRoot:      stageRoot,
		RuntimeKind:   run.RuntimeKind,
	}); err != nil {
		return "", skills.Resolution{}, fmt.Errorf("stage runtime skills: %w", err)
	}
	reconciled, err := skills.ReconcileResolutionFromStagedPackages(stagingResolution, stageRoot)
	if err != nil {
		return "", skills.Resolution{}, fmt.Errorf("reconcile staged runtime skills: %w", err)
	}
	_, allowedTools, err := e.skillPolicyForRun(ctx, agent, run)
	if err != nil {
		return "", skills.Resolution{}, err
	}
	if err := skills.ValidateRuntimeAndTools(agent.RuntimeKind, allowedTools, reconciled.Definitions); err != nil {
		return "", skills.Resolution{}, err
	}
	if resolution.UsesExplicitRoles {
		resolution.AvailableDefinitions = append([]skills.Definition(nil), reconciled.Definitions...)
	} else {
		resolution = reconciled
	}
	if err := e.persistRuntimeSkillManifest(ctx, run, stageRoot, resolution); err != nil {
		return "", skills.Resolution{}, err
	}
	stageRepositorySkills(run, lease, stageRoot, resolution)
	if targetContext != nil && targetContext.Data != nil {
		targetContext.Data["staged_skill_root"] = stageRoot
	}
	return stageRoot, resolution, nil
}

// stageRepositorySkillsOnly handles runs with no host-provided skills: the
// checkout may still ship `.agents/skills`, which become available skills.
func (e *Engine) stageRepositorySkillsOnly(run *agentcore.AgentRun, resolution skills.Resolution, lease *agentcore.WorkspaceLease, targetContext *host.TargetContext) (string, skills.Resolution, error) {
	if lease == nil || strings.TrimSpace(lease.RootPath) == "" {
		return "", resolution, nil
	}
	stageRoot := stagedSkillRootPath(run, lease)
	if !stageRepositorySkills(run, lease, stageRoot, resolution) {
		return "", resolution, nil
	}
	if targetContext != nil && targetContext.Data != nil {
		targetContext.Data["staged_skill_root"] = stageRoot
	}
	return stageRoot, resolution, nil
}

// stageRepositorySkills adds the checkout's `.agents/skills` to the staged
// catalog. Host-owned keys win on collision. Failures are logged and never
// fail the run; repository skills are optional context. Returns whether any
// repository skill was staged.
func stageRepositorySkills(run *agentcore.AgentRun, lease *agentcore.WorkspaceLease, stageRoot string, resolution skills.Resolution) bool {
	if lease == nil || strings.TrimSpace(lease.RootPath) == "" || strings.TrimSpace(stageRoot) == "" {
		return false
	}
	reserved := map[string]bool{}
	for _, definition := range resolution.Definitions {
		reserved[definition.Key] = true
	}
	for _, definition := range resolution.AvailableDefinitions {
		reserved[definition.Key] = true
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	report, err := skills.StageRepositorySkills(lease.RootPath, stageRoot, reserved)
	if err != nil {
		slog.Warn("repository skills were not staged", "run_id", runID, "error", err)
		return false
	}
	if len(report.Staged) > 0 || len(report.Skipped) > 0 || report.Limited {
		slog.Info("repository skills staged", "run_id", runID, "staged", report.Staged, "skipped", report.Skipped, "limited", report.Limited)
	}
	return len(report.Staged) > 0
}

func stagedSkillRootPath(run *agentcore.AgentRun, lease *agentcore.WorkspaceLease) string {
	if lease != nil && strings.TrimSpace(lease.RootPath) != "" {
		return filepath.Join(strings.TrimSpace(lease.RootPath), ".agent-runtime", "skills")
	}
	runID := "run"
	if run != nil && strings.TrimSpace(run.ID) != "" {
		runID = strings.TrimSpace(run.ID)
	}
	return filepath.Join(os.TempDir(), "agent-runtime-skills", sanitizeSkillRootComponent(runID), "skills")
}

func sanitizeSkillRootComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "run"
	}
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	out := strings.Trim(builder.String(), "_")
	if out == "" {
		return "run"
	}
	return out
}

func (e *Engine) persistRuntimeSkillManifest(ctx context.Context, run *agentcore.AgentRun, stageRoot string, resolution skills.Resolution) error {
	if e == nil || e.cfg.Store == nil || run == nil || strings.TrimSpace(stageRoot) == "" {
		return nil
	}
	type manifestEntry struct {
		Key        string `json:"key"`
		SourceKind string `json:"source_kind"`
		VersionKey string `json:"version_key,omitempty"`
		SkillID    string `json:"skill_id,omitempty"`
	}
	manifestRefs := resolution.CoreRefs
	manifestDefinitions := resolution.Definitions
	policyDefinitions := resolution.Definitions
	if resolution.UsesExplicitRoles {
		manifestRefs = resolution.AvailableRefs
		manifestDefinitions = resolution.AvailableDefinitions
		policyDefinitions = resolution.InstructionDefinitions
	}
	entries := make([]manifestEntry, 0, len(manifestRefs))
	for idx, ref := range manifestRefs {
		definition := skills.Definition{}
		if idx < len(manifestDefinitions) {
			definition = manifestDefinitions[idx]
		}
		entries = append(entries, manifestEntry{
			Key:        definition.Key,
			SourceKind: definition.SourceKind,
			VersionKey: ref.VersionKey,
			SkillID:    ref.SkillID,
		})
	}
	payload, err := json.Marshal(map[string]interface{}{
		"runtime_kind":                          run.RuntimeKind,
		"staged_root":                           stageRoot,
		"skills":                                entries,
		"completion_requires_interaction_kinds": skills.CompletionRequiredInteractionKinds(resolution.Policy, policyDefinitions),
	})
	if err != nil {
		return fmt.Errorf("marshal runtime skill manifest: %w", err)
	}
	return e.cfg.Store.AppendArtifact(ctx, &agentcore.AgentRunArtifact{
		AppID:         run.AppID,
		RunID:         run.ID,
		ArtifactType:  "runtime_skill_manifest",
		Format:        "json",
		StorageMode:   "inline",
		InlineContent: string(payload),
	})
}

func (e *Engine) ExecuteRunOnce(ctx context.Context, appID, runID string) (*runtime.Result, error) {
	run, err := e.cfg.Store.GetRun(ctx, appID, runID)
	if err != nil || run == nil || agentcore.IsTerminalStatus(run.Status) {
		return nil, err
	}
	defer e.closeTerminalRunToolResources(ctx, run)
	agent, err := e.cfg.Store.GetAgent(ctx, run.AppID, run.AgentID)
	if err != nil || agent == nil {
		e.failRun(ctx, run, "agent not found")
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("agent not found")
	}
	if err := e.executionPolicy(agent, run); err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	if run.ApprovalState == agentcore.ApprovalPending {
		run.Status = agentcore.RunStatusPaused
		run.PauseReason = agentcore.PauseReasonHumanApproval
		_ = e.cfg.Store.UpdateRun(ctx, run)
		e.emitRunEvent(ctx, run, "run.paused", map[string]interface{}{"pause_reason": run.PauseReason})
		return &runtime.Result{WaitForApproval: true}, nil
	}
	targetContext, err := e.resolveTargetForRun(ctx, host.TargetContextRequest{
		AppID:    run.AppID,
		RunID:    run.ID,
		AgentID:  run.AgentID,
		Target:   run.Target,
		Trigger:  run.Input.Trigger,
		Metadata: run.Input.Metadata,
	})
	if err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	now := time.Now().UTC()
	run.Status = agentcore.RunStatusRunning
	run.PauseReason = agentcore.PauseReasonNone
	run.StartedAt = &now
	if run.Input.Metadata == nil {
		run.Input.Metadata = map[string]interface{}{}
	}
	if _, ok := run.Input.Metadata["turn_started_at"]; !ok {
		run.Input.Metadata["turn_started_at"] = now.Format(time.RFC3339Nano)
	}
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	e.emitRunEvent(ctx, run, "run.started", nil)
	adapter, err := e.cfg.Runtimes.Get(run.RuntimeKind)
	if err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	skillResolution, err := e.resolveSkills(ctx, agent, run)
	if err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	configuredRuntimePolicy, err := runtimePolicyFromExecutionConfig(agent.ExecutionConfig)
	if err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	skillResolution.Policy = mergeRuntimePolicy(skillResolution.Policy, configuredRuntimePolicy)
	workspaceLease, err := e.ensureWorkspace(ctx, agent, run, targetContext)
	if err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	stagedSkillRoot, skillResolution, err := e.stageRuntimeSkills(ctx, agent, run, skillResolution, workspaceLease, targetContext)
	if err != nil {
		e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusFailed, err.Error(), nil)
		e.cleanupWorkspace(ctx, run, "failed", true)
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	runTools, runMCPAllowed, runMCPAuth, closeRunMCP, err := mcp.PrepareRunTools(ctx, e.cfg.Store, e.cfg.Tools, run.AppID, run.ID, e.cfg.RunMCP)
	if err != nil {
		var authenticationErr *mcp.AuthenticationError
		if errors.As(err, &authenticationErr) {
			if pauseErr := e.pauseForMCPAuthentication(ctx, run, workspaceLease, authenticationErr); pauseErr != nil {
				e.failRun(ctx, run, pauseErr.Error())
				return nil, pauseErr
			}
			return &runtime.Result{AwaitingAuth: true}, nil
		}
		e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusFailed, err.Error(), nil)
		e.cleanupWorkspace(ctx, run, "failed", true)
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	defer closeRunMCP()
	allowedTools := tools.AllowedSet(agent, run.Input.AllowedTools)
	for name := range runMCPAllowed {
		allowedTools[name] = true
	}
	activeSkillRefs := skillResolution.CoreRefs
	activeSkillDefinitions := skillResolution.Definitions
	if skillResolution.UsesExplicitRoles {
		activeSkillRefs = skillResolution.InstructionRefs
		activeSkillDefinitions = skillResolution.InstructionDefinitions
	}
	execCtx := &runtime.ExecutionContext{
		ModelCredentials:          e.cfg.ModelCredentials,
		Context:                   ctx,
		AppID:                     run.AppID,
		Agent:                     agent,
		Run:                       run,
		Store:                     e.cfg.Store,
		TargetContext:             targetContext,
		WorkspaceLease:            workspaceLease,
		AllowedTools:              allowedTools,
		Tools:                     runTools,
		WorkspaceManager:          engineWorkspaceManager{engine: e, agent: agent, run: run, targetContext: targetContext},
		SkillRefs:                 activeSkillRefs,
		SkillDefinitions:          activeSkillDefinitions,
		AvailableSkillRefs:        append([]agentcore.SkillRef(nil), skillResolution.AvailableRefs...),
		AvailableSkillDefinitions: append([]skills.Definition(nil), skillResolution.AvailableDefinitions...),
		SkillInstructions:         skillResolution.Instructions,
		SkillPolicy:               skillResolution.Policy,
		UsesSplitSkills:           skillResolution.UsesExplicitRoles,
		StagedSkillRoot:           stagedSkillRoot,
		ArtifactWriter:            artifactWriter{store: e.cfg.Store, run: run},
		InteractionBroker:         interactionBroker{store: e.cfg.Store, run: run},
		EventSink:                 runtimeEventSink{sink: e.cfg.EventSink, hostRunID: run.HostRunID},
	}
	result, err := adapter.Execute(execCtx)
	if usageErr := e.recoverNativeUsage(ctx, run); usageErr != nil && err == nil {
		err = usageErr
	}
	// A worker shutdown cancels the activity context. Leave the durable run and
	// workspace intact so Temporal can retry it on another worker; treating this
	// infrastructure interruption as an agent failure makes routine deploys
	// terminalize healthy runs.
	if executionContextInterrupted(ctx, err) {
		return nil, err
	}
	if stored, terminal, terminalErr := e.currentTerminalRun(ctx, run); terminalErr != nil {
		return nil, terminalErr
	} else if terminal {
		run = stored
		e.cleanupWorkspace(ctx, run, strings.TrimSpace(run.Status), true)
		return result, nil
	}
	var modelAuthErr *modelauth.AuthenticationError
	if errors.As(err, &modelAuthErr) {
		if pauseErr := e.pauseForModelAuthentication(ctx, run, modelAuthErr); pauseErr != nil {
			return nil, pauseErr
		}
		return &runtime.Result{AwaitingAuth: true}, nil
	}
	if authenticationErr := runMCPAuth.Failure(); authenticationErr != nil {
		if pauseErr := e.pauseForMCPAuthentication(ctx, run, workspaceLease, authenticationErr); pauseErr != nil {
			e.failRun(ctx, run, pauseErr.Error())
			return nil, pauseErr
		}
		return &runtime.Result{AwaitingAuth: true}, nil
	}
	if err != nil {
		e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusFailed, err.Error(), nil)
		e.cleanupWorkspace(ctx, run, "failed", true)
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	if result == nil {
		result = &runtime.Result{}
	}
	if err := e.applyPendingInteractionState(ctx, run, result); err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	result, err = e.enforceCompletionInteractionPolicy(ctx, adapter, execCtx, run, skillResolution, result)
	if usageErr := e.recoverNativeUsage(ctx, run); usageErr != nil && err == nil {
		err = usageErr
	}
	if executionContextInterrupted(ctx, err) {
		return nil, err
	}
	if errors.As(err, &modelAuthErr) {
		if pauseErr := e.pauseForModelAuthentication(ctx, run, modelAuthErr); pauseErr != nil {
			return nil, pauseErr
		}
		return &runtime.Result{AwaitingAuth: true}, nil
	}
	if err != nil {
		e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusFailed, err.Error(), nil)
		e.cleanupWorkspace(ctx, run, "failed", true)
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	if err := validateExplicitTurnCompletion(run, result); err != nil {
		e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusFailed, err.Error(), result.OutputSummary)
		e.cleanupWorkspace(ctx, run, "failed", true)
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	result.OutputSummary = cumulativeOutputSummary(run.OutputSummary, result.OutputSummary, run.RuntimeKind)
	e.emitUsageCheckpoint(ctx, run, result.OutputSummary)
	if result.AssistantMessage != "" && !result.MessagesPersisted {
		_ = e.cfg.Store.AppendMessage(ctx, &agentcore.AgentRunMessage{
			AppID:            run.AppID,
			RunID:            run.ID,
			RuntimeMessageID: result.AssistantMessageID,
			Role:             "assistant",
			Content:          result.AssistantMessage,
			MessageType:      "message",
		})
	}
	if result.WaitForApproval || result.AwaitingInput || result.AwaitingAuth {
		if len(result.OutputSummary) > 0 {
			run.OutputSummary = result.OutputSummary
		}
		if err := e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusPaused, "", result.OutputSummary); err != nil {
			e.failRun(ctx, run, err.Error())
			return nil, err
		}
		if stored, terminal, terminalErr := e.currentTerminalRun(ctx, run); terminalErr != nil {
			return nil, terminalErr
		} else if terminal {
			run = stored
			e.cleanupWorkspace(ctx, run, strings.TrimSpace(run.Status), true)
			return result, nil
		}
		e.cleanupWorkspace(ctx, run, "paused", false)
		run.Status = agentcore.RunStatusPaused
		switch {
		case result.WaitForApproval:
			run.PauseReason = agentcore.PauseReasonHumanApproval
		case result.AwaitingAuth:
			run.PauseReason = agentcore.PauseReasonAuth
		default:
			run.PauseReason = agentcore.PauseReasonHumanInput
		}
		_ = e.cfg.Store.UpdateRun(ctx, run)
		e.emitRunEvent(ctx, run, "run.paused", map[string]interface{}{"pause_reason": run.PauseReason})
		return result, nil
	}
	if agentcore.ShouldPauseAfterAssistant(run) {
		if len(result.OutputSummary) > 0 {
			run.OutputSummary = result.OutputSummary
		}
		if err := e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusPaused, "", result.OutputSummary); err != nil {
			e.failRun(ctx, run, err.Error())
			return nil, err
		}
		if stored, terminal, terminalErr := e.currentTerminalRun(ctx, run); terminalErr != nil {
			return nil, terminalErr
		} else if terminal {
			run = stored
			e.cleanupWorkspace(ctx, run, strings.TrimSpace(run.Status), true)
			return result, nil
		}
		e.cleanupWorkspace(ctx, run, "paused", false)
		run.Status = agentcore.RunStatusPaused
		run.PauseReason = agentcore.PauseReasonUserMessage
		run.CompletedAt = nil
		if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
			e.failRun(ctx, run, err.Error())
			return nil, err
		}
		e.emitRunEvent(ctx, run, "run.paused", map[string]interface{}{"pause_reason": run.PauseReason})
		result.AwaitingInput = true
		return result, nil
	}
	if err := e.validateCompletionContract(ctx, agent, run); err != nil {
		e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusFailed, err.Error(), result.OutputSummary)
		e.cleanupWorkspace(ctx, run, "failed", true)
		e.failRun(ctx, run, err.Error())
		return nil, err
	}

	completedAt := time.Now().UTC()
	if len(result.OutputSummary) > 0 {
		run.OutputSummary = result.OutputSummary
	}
	if err := e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusCompleted, "", result.OutputSummary); err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	if stored, terminal, terminalErr := e.currentTerminalRun(ctx, run); terminalErr != nil {
		return nil, terminalErr
	} else if terminal {
		run = stored
		e.cleanupWorkspace(ctx, run, strings.TrimSpace(run.Status), true)
		return result, nil
	}
	run.Status = agentcore.RunStatusCompleted
	run.PauseReason = agentcore.PauseReasonNone
	run.CompletedAt = &completedAt
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	e.clearRunCredentials(ctx, run)
	e.cleanupWorkspace(ctx, run, "completed", true)
	e.emitRunEvent(ctx, run, "run.completed", e.terminalEventData(run, nil))
	return result, nil
}

func (e *Engine) closeRunToolResources(ctx context.Context, run *agentcore.AgentRun) {
	if e == nil || e.cfg.Tools == nil || run == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
	defer cancel()
	if err := e.cfg.Tools.CloseRun(cleanupCtx, run.AppID, run.ID); err != nil {
		slog.ErrorContext(cleanupCtx, "run tool resource cleanup failed", "app_id", run.AppID, "run_id", run.ID, "error", err)
	}
}

// closeTerminalRunToolResources keeps ephemeral tool state, such as a browser
// session, alive while a run is paused for approval, authentication, or user
// input. The tool family remains responsible for its own idle timeout.
func (e *Engine) closeTerminalRunToolResources(ctx context.Context, run *agentcore.AgentRun) {
	if e == nil || run == nil {
		return
	}
	terminal := agentcore.IsTerminalStatus(run.Status)
	if !terminal && e.cfg.Store != nil {
		if stored, err := e.cfg.Store.GetRun(context.WithoutCancel(ctx), run.AppID, run.ID); err == nil && stored != nil {
			terminal = agentcore.IsTerminalStatus(stored.Status)
		}
	}
	if terminal {
		e.closeRunToolResources(ctx, run)
	}
}

func (e *Engine) pauseForMCPAuthentication(
	ctx context.Context,
	run *agentcore.AgentRun,
	lease *agentcore.WorkspaceLease,
	authErr *mcp.AuthenticationError,
) error {
	if run == nil || authErr == nil {
		return fmt.Errorf("MCP authentication pause requires a run and error")
	}
	requestPayload, err := json.Marshal(map[string]interface{}{
		"provider":    "mcp",
		"server_id":   authErr.ServerID,
		"server_name": authErr.ServerName,
		"reason":      authErr.Reason,
	})
	if err != nil {
		return err
	}
	if err := e.cfg.Store.AppendInteraction(ctx, &agentcore.AgentRunInteraction{
		AppID: run.AppID, RunID: run.ID, RuntimeKind: run.RuntimeKind,
		InteractionKind: "authentication", Status: "pending",
		Title:          "External MCP authentication required",
		Summary:        "Reconnect " + authErr.ServerName + " to continue this run.",
		RequestPayload: requestPayload,
	}); err != nil {
		return err
	}
	if err := e.finalizeWorkspace(
		ctx, run, lease, agentcore.RunStatusPaused, "", run.OutputSummary,
	); err != nil {
		return err
	}
	e.cleanupWorkspace(ctx, run, "paused", false)
	run.Status = agentcore.RunStatusPaused
	run.PauseReason = agentcore.PauseReasonAuth
	run.ErrorMessage = ""
	run.CompletedAt = nil
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		return err
	}
	e.emitRunEvent(ctx, run, "run.paused", map[string]interface{}{
		"pause_reason": run.PauseReason,
		"authentication": map[string]interface{}{
			"provider": "mcp", "server_id": authErr.ServerID,
			"server_name": authErr.ServerName, "reason": authErr.Reason,
		},
	})
	return nil
}

func (e *Engine) applyPendingInteractionState(ctx context.Context, run *agentcore.AgentRun, result *runtime.Result) error {
	if e == nil || e.cfg.Store == nil || run == nil || result == nil {
		return nil
	}
	interactions, err := e.cfg.Store.ListInteractions(ctx, run.AppID, run.ID)
	if err != nil {
		return err
	}
	for i := len(interactions) - 1; i >= 0; i-- {
		interaction := interactions[i]
		if strings.TrimSpace(interaction.Status) != "pending" {
			continue
		}
		switch strings.TrimSpace(interaction.InteractionKind) {
		case "approval_request", "review_checkpoint", "human_approval":
			result.WaitForApproval = true
			return nil
		case "request_user_input", "input_request", "human_input":
			result.AwaitingInput = true
			return nil
		}
	}
	return nil
}

// completionInteractionKindAliases maps a policy-required interaction kind to
// every kind string the runtimes actually persist for it: the native adapter
// uses the canonical names, while the Codex adapter's built-in pauses record
// human_input/human_approval.
var completionInteractionKindAliases = map[string][]string{
	"request_user_input": {"request_user_input", "input_request", "human_input"},
	"approval_request":   {"approval_request", "request_approval", "human_approval"},
	"review_checkpoint":  {"review_checkpoint"},
}

// enforceCompletionInteractionPolicy is the backstop for skills that declare
// completion_requires_interaction_kinds: a run whose active skills require a
// human interaction must not complete without one ever happening. When a turn
// tries to complete anyway, run one corrective follow-up turn telling the
// model to use its interaction tools; if that still produces no interaction,
// fail the run instead of completing it silently.
func (e *Engine) enforceCompletionInteractionPolicy(ctx context.Context, adapter runtime.Adapter, execCtx *runtime.ExecutionContext, run *agentcore.AgentRun, resolution skills.Resolution, result *runtime.Result) (*runtime.Result, error) {
	requiredKinds := skills.CompletionRequiredInteractionKinds(resolution.Policy, resolution.Definitions)
	if len(requiredKinds) == 0 || result == nil || run == nil {
		return result, nil
	}
	if result.WaitForApproval || result.AwaitingInput || result.AwaitingAuth || agentcore.ShouldPauseAfterAssistant(run) {
		return result, nil
	}
	satisfied, err := e.completionInteractionPolicySatisfied(ctx, run, requiredKinds)
	if err != nil {
		return result, err
	}
	if satisfied {
		return result, nil
	}
	slog.WarnContext(ctx, "run tried to complete without a required interaction; starting corrective turn",
		"run_id", run.ID,
		"required_interaction_kinds", requiredKinds,
	)
	// Keep the first attempt's visible output before the corrective turn's
	// result replaces it.
	if result.AssistantMessage != "" && !result.MessagesPersisted {
		_ = e.cfg.Store.AppendMessage(ctx, &agentcore.AgentRunMessage{
			AppID:            run.AppID,
			RunID:            run.ID,
			RuntimeMessageID: result.AssistantMessageID,
			Role:             "assistant",
			Content:          result.AssistantMessage,
			MessageType:      "message",
		})
	}
	originalInstructions := run.Input.Instructions
	run.Input.Instructions = completionInteractionCorrectivePrompt(requiredKinds)
	corrected, execErr := adapter.Execute(execCtx)
	run.Input.Instructions = originalInstructions
	if execErr != nil {
		return result, execErr
	}
	if corrected == nil {
		corrected = &runtime.Result{}
	}
	corrected.OutputSummary = cumulativeOutputSummary(result.OutputSummary, corrected.OutputSummary, run.RuntimeKind)
	if err := e.applyPendingInteractionState(ctx, run, corrected); err != nil {
		return corrected, err
	}
	if corrected.WaitForApproval || corrected.AwaitingInput || corrected.AwaitingAuth {
		return corrected, nil
	}
	satisfied, err = e.completionInteractionPolicySatisfied(ctx, run, requiredKinds)
	if err != nil {
		return corrected, err
	}
	if satisfied {
		return corrected, nil
	}
	return corrected, fmt.Errorf("run cannot complete because active skills require one of [%s] before completion", strings.Join(requiredKinds, ", "))
}

func validateExplicitTurnCompletion(run *agentcore.AgentRun, result *runtime.Result) error {
	if !agentcore.RequiresExplicitTurnFinish(run) || result == nil {
		return nil
	}
	if result.WaitForApproval || result.AwaitingInput || result.AwaitingAuth {
		return nil
	}
	if result.TurnFinished {
		return nil
	}
	return fmt.Errorf("turn_completion_guard_exhausted: runtime adapter %q returned without a valid finish_turn or interaction pause", run.RuntimeKind)
}

func (e *Engine) completionInteractionPolicySatisfied(ctx context.Context, run *agentcore.AgentRun, requiredKinds []string) (bool, error) {
	interactions, err := e.cfg.Store.ListInteractions(ctx, run.AppID, run.ID)
	if err != nil {
		return false, fmt.Errorf("verify completion interaction requirements: %w", err)
	}
	accepted := make(map[string]bool, len(requiredKinds)*3)
	for _, kind := range requiredKinds {
		aliases, ok := completionInteractionKindAliases[kind]
		if !ok {
			aliases = []string{kind}
		}
		for _, alias := range aliases {
			accepted[alias] = true
		}
	}
	for _, interaction := range interactions {
		if accepted[strings.TrimSpace(interaction.InteractionKind)] {
			return true, nil
		}
	}
	return false, nil
}

func completionInteractionCorrectivePrompt(requiredKinds []string) string {
	toolNames := make([]string, 0, len(requiredKinds))
	for _, kind := range requiredKinds {
		switch kind {
		case "approval_request":
			toolNames = append(toolNames, "request_approval")
		case "request_user_input":
			toolNames = append(toolNames, "request_user_input")
		case "review_checkpoint":
			toolNames = append(toolNames, "request_review_checkpoint")
		default:
			toolNames = append(toolNames, kind)
		}
	}
	return strings.Join([]string{
		"You ended your previous turn without the required human interaction, so the run cannot complete yet.",
		fmt.Sprintf("The active skills require you to pause for the human using one of these tools before finishing: %s.", strings.Join(toolNames, ", ")),
		"Call the appropriate interaction tool now for the work you already produced (for example, request approval of the draft you published). Do not ask for approval in prose, and do not finish the run without calling one of those tools.",
	}, "\n")
}

func executionContextInterrupted(ctx context.Context, err error) bool {
	if ctx == nil || ctx.Err() == nil || err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (e *Engine) validateCompletionContract(ctx context.Context, agent *agentcore.Agent, run *agentcore.AgentRun) error {
	required := completion.RequiredTools(agent, run)
	if len(required) == 0 || run == nil {
		return nil
	}
	calls, err := e.cfg.Store.ListToolCalls(ctx, run.AppID, run.ID)
	if err != nil {
		return fmt.Errorf("list completion tool calls: %w", err)
	}
	succeeded := make(map[string]bool, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.Error) == "" {
			succeeded[tools.CanonicalName(call.ToolName)] = true
		}
	}
	missing := make([]string, 0, len(required))
	for _, toolName := range required {
		toolName = tools.CanonicalName(toolName)
		if toolName != "" && !succeeded[toolName] {
			missing = append(missing, toolName)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("run completion requires successful tool calls: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (e *Engine) PrepareRunOnce(ctx context.Context, appID, runID string) error {
	run, err := e.cfg.Store.GetRun(ctx, appID, runID)
	if err != nil || run == nil || agentcore.IsTerminalStatus(run.Status) {
		return err
	}
	agent, err := e.cfg.Store.GetAgent(ctx, run.AppID, run.AgentID)
	if err != nil || agent == nil {
		e.failRun(ctx, run, "agent not found")
		if err != nil {
			return err
		}
		return fmt.Errorf("agent not found")
	}
	if err := e.executionPolicy(agent, run); err != nil {
		e.failRun(ctx, run, err.Error())
		return err
	}
	if run.ApprovalState == agentcore.ApprovalPending {
		run.Status = agentcore.RunStatusPaused
		run.PauseReason = agentcore.PauseReasonHumanApproval
		_ = e.cfg.Store.UpdateRun(ctx, run)
		e.emitRunEvent(ctx, run, "run.paused", map[string]interface{}{"pause_reason": run.PauseReason})
		return nil
	}
	targetContext, err := e.resolveTargetForRun(ctx, host.TargetContextRequest{
		AppID:    run.AppID,
		RunID:    run.ID,
		AgentID:  run.AgentID,
		Target:   run.Target,
		Trigger:  run.Input.Trigger,
		Metadata: run.Input.Metadata,
	})
	if err != nil {
		e.failRun(ctx, run, err.Error())
		return err
	}
	if _, err := e.ensureWorkspace(ctx, agent, run, targetContext); err != nil {
		e.failRun(ctx, run, err.Error())
		return err
	}
	if run.Status == agentcore.RunStatusQueued {
		now := time.Now().UTC()
		run.Status = agentcore.RunStatusRunning
		run.PauseReason = agentcore.PauseReasonNone
		if run.StartedAt == nil {
			run.StartedAt = &now
		}
		if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
			e.failRun(ctx, run, err.Error())
			return err
		}
	}
	return nil
}

// Recover accounting independently of run status. In particular, never write a
// stale run record over cancellation just to checkpoint token usage.
func (e *Engine) recoverNativeUsage(ctx context.Context, run *agentcore.AgentRun) error {
	if run == nil || run.RuntimeKind != agentcore.RuntimeNativeSDK {
		return nil
	}
	usageCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	usageSummary, err := runtime.NativeCheckpointUsage(usageCtx, e.cfg.Store, run.AppID, run.ID)
	if err != nil {
		return err
	}
	if len(usageSummary) > 0 {
		run.OutputSummary = mergeOutputSummaries(run.OutputSummary, usageSummary)
		e.emitUsageCheckpoint(usageCtx, run, usageSummary)
	}
	return nil
}

func (e *Engine) currentTerminalRun(ctx context.Context, run *agentcore.AgentRun) (*agentcore.AgentRun, bool, error) {
	if e == nil || e.cfg.Store == nil || run == nil {
		return run, false, nil
	}
	stored, err := e.cfg.Store.GetRun(ctx, run.AppID, run.ID)
	if err != nil || stored == nil {
		return stored, false, err
	}
	return stored, agentcore.IsTerminalStatus(stored.Status), nil
}

type engineWorkspaceManager struct {
	engine        *Engine
	agent         *agentcore.Agent
	run           *agentcore.AgentRun
	targetContext *host.TargetContext
}

// SetRepositoryBranch keeps the durable lease in step with create_branch.
// Repository validation checks the recorded work branch on every resume, so
// persisting this change is part of the branch operation rather than optional
// bookkeeping.
func (m engineWorkspaceManager) SetRepositoryBranch(ctx context.Context, branch string) error {
	if m.engine == nil || m.run == nil || m.run.WorkspaceLease == nil {
		return fmt.Errorf("repository branch update requires an active workspace")
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("repository branch is required")
	}
	lease := m.run.WorkspaceLease
	if workspace.RepositorySpecFromLease(*lease) != nil {
		if err := workspace.UpdateRepositoryLeaseBranch(lease, branch); err != nil {
			return err
		}
	} else {
		// Host-prepared and local CLI leases carry no repository spec. Record
		// the branch on the keys that exist so resumes see the same checkout.
		if lease.Metadata == nil {
			lease.Metadata = map[string]interface{}{}
		}
		lease.Metadata["work_branch"] = branch
		delete(lease.Metadata, "detached_head")
	}
	if m.run.Input.Metadata == nil {
		m.run.Input.Metadata = map[string]interface{}{}
	}
	m.run.Input.Metadata["work_branch"] = branch
	delete(m.run.Input.Metadata, "detached_head")
	return m.persistWorkspaceState(ctx)
}

// SetRepositoryDetachedHead keeps a deliberate detached checkout or an
// in-progress Git operation intact across durable activity resumes.
func (m engineWorkspaceManager) SetRepositoryDetachedHead(ctx context.Context, commit string) error {
	if m.engine == nil || m.run == nil || m.run.WorkspaceLease == nil {
		return fmt.Errorf("repository detached HEAD update requires an active workspace")
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("detached repository commit is required")
	}
	lease := m.run.WorkspaceLease
	if workspace.RepositorySpecFromLease(*lease) != nil {
		if err := workspace.UpdateRepositoryLeaseDetachedHead(lease, commit); err != nil {
			return err
		}
	} else {
		if lease.Metadata == nil {
			lease.Metadata = map[string]interface{}{}
		}
		delete(lease.Metadata, "work_branch")
		delete(lease.Metadata, "branch_sync_work_branch")
		lease.Metadata["detached_head"] = commit
	}
	if m.run.Input.Metadata == nil {
		m.run.Input.Metadata = map[string]interface{}{}
	}
	delete(m.run.Input.Metadata, "work_branch")
	m.run.Input.Metadata["detached_head"] = commit
	return m.persistWorkspaceState(ctx)
}

// persistWorkspaceState writes the manager's lease and run metadata onto the
// run as currently stored. The worker's in-memory run may be stale: a
// concurrent CancelRun already closed the run, and saving the stale copy
// would reopen it.
func (m engineWorkspaceManager) persistWorkspaceState(ctx context.Context) error {
	if m.engine == nil || m.engine.cfg.Store == nil || m.run == nil {
		return fmt.Errorf("workspace state update requires an active run")
	}
	stored, err := m.engine.cfg.Store.GetRun(ctx, m.run.AppID, m.run.ID)
	if err != nil {
		return err
	}
	if stored == nil {
		return fmt.Errorf("run not found")
	}
	if agentcore.IsTerminalStatus(stored.Status) {
		return fmt.Errorf("run is no longer active")
	}
	stored.WorkspaceLease = m.run.WorkspaceLease
	stored.Input.Metadata = m.run.Input.Metadata
	return m.engine.cfg.Store.UpdateRun(ctx, stored)
}

func (m engineWorkspaceManager) CheckoutRepository(ctx context.Context, req tools.CheckoutRepositoryRequest) (*tools.CheckoutRepositoryResult, error) {
	if m.engine == nil || m.run == nil || m.agent == nil {
		return nil, fmt.Errorf("repository checkout requires an active run")
	}
	if m.engine.cfg.Workspaces == nil {
		return nil, fmt.Errorf("repository workspace requested but workspace registry is not configured")
	}
	provider, ok := m.engine.cfg.Workspaces.Provider(m.run.AppID)
	if !ok {
		return nil, fmt.Errorf("repository workspace requested but no workspace provider is configured for app %q", m.run.AppID)
	}
	target := repositoryCheckoutTarget(m.run, req)
	lease, err := provider.PrepareWorkspace(ctx, workspace.PrepareRequest{
		AppID:           m.run.AppID,
		RunID:           m.run.ID,
		AgentID:         m.run.AgentID,
		RuntimeKind:     m.run.RuntimeKind,
		Target:          target,
		TargetContext:   m.targetContext,
		Instructions:    m.run.Input.Instructions,
		Trigger:         m.run.Input.Trigger,
		Metadata:        m.run.Input.Metadata,
		WorkspaceMode:   workspace.ModeRepository,
		ExecutionConfig: m.agent.ExecutionConfig,
	})
	if err != nil {
		return nil, err
	}
	workspace.NormalizeLease(lease)
	if lease == nil || strings.TrimSpace(lease.RootPath) == "" {
		return nil, fmt.Errorf("repository checkout did not return a workspace root")
	}
	alias := repositoryCheckoutAlias(req, lease)
	primary := req.Primary || m.run.WorkspaceLease == nil || strings.TrimSpace(m.run.WorkspaceLease.RootPath) == "" || m.run.WorkspaceLease.Provider == "analysis"

	if primary {
		markPrimaryRepositoryWorkspace(m.run, req, lease, alias)
		m.run.WorkspaceLease = lease
	} else {
		if m.run.WorkspaceLease.Metadata == nil {
			m.run.WorkspaceLease.Metadata = map[string]interface{}{}
		}
		m.run.WorkspaceLease.Metadata["repository_workspaces"] = upsertRepositoryWorkspaceEntry(m.run.WorkspaceLease.Metadata["repository_workspaces"], alias, lease)
	}
	if err := m.engine.cfg.Store.UpdateRun(ctx, m.run); err != nil {
		return nil, err
	}
	data := map[string]interface{}{"lease_id": lease.ID, "provider": lease.Provider, "metadata": lease.Metadata, "primary": primary}
	if alias != "" {
		data["alias"] = alias
	}
	m.engine.emitRunEvent(ctx, m.run, "workspace.prepared", data)
	return &tools.CheckoutRepositoryResult{
		Alias:        alias,
		Primary:      primary,
		Lease:        lease,
		RepositoryID: stringFromMap(lease.Metadata, "repository_id"),
		RepoFullName: stringFromMap(lease.Metadata, "repo_full_name"),
		BaseBranch:   stringFromMap(lease.Metadata, "base_branch"),
		WorkBranch:   stringFromMap(lease.Metadata, "work_branch"),
	}, nil
}

func repositoryCheckoutTarget(run *agentcore.AgentRun, req tools.CheckoutRepositoryRequest) agentcore.TargetRef {
	if run == nil {
		return agentcore.TargetRef{}
	}
	if strings.TrimSpace(req.RepositoryID) == "" && strings.TrimSpace(req.RepoFullName) == "" {
		return run.Target
	}
	metadata := copyStringAnyMap(run.Target.Metadata)
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	for key, value := range run.Input.Metadata {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	if strings.TrimSpace(req.RepoFullName) != "" {
		metadata["repo_full_name"] = strings.TrimSpace(req.RepoFullName)
	}
	if strings.TrimSpace(req.BaseBranch) != "" {
		metadata["base_branch"] = strings.TrimSpace(req.BaseBranch)
	}
	if strings.TrimSpace(req.WorkBranch) != "" {
		metadata["work_branch"] = strings.TrimSpace(req.WorkBranch)
	}
	if strings.TrimSpace(req.Alias) != "" {
		metadata["repo_alias"] = strings.TrimSpace(req.Alias)
	}
	return agentcore.TargetRef{
		Type:     "repository",
		ID:       firstNonEmpty(req.RepositoryID, req.RepoFullName),
		Metadata: metadata,
	}
}

func repositoryCheckoutAlias(req tools.CheckoutRepositoryRequest, lease *agentcore.WorkspaceLease) string {
	return firstNonEmpty(req.Alias, stringFromMap(lease.Metadata, "repo_alias"), stringFromMap(lease.Metadata, "repo_full_name"), stringFromMap(lease.Metadata, "repository_id"), lease.ID)
}

func upsertRepositoryWorkspaceEntry(raw interface{}, alias string, lease *agentcore.WorkspaceLease) map[string]interface{} {
	entries, _ := raw.(map[string]interface{})
	if entries == nil {
		entries = map[string]interface{}{}
	}
	key := strings.TrimSpace(alias)
	if key == "" && lease != nil {
		key = strings.TrimSpace(lease.ID)
	}
	entry := map[string]interface{}{
		"id":        lease.ID,
		"provider":  lease.Provider,
		"root_path": lease.RootPath,
		"metadata":  copyStringAnyMap(lease.Metadata),
	}
	if key != "" {
		entry["alias"] = key
	}
	for _, metaKey := range []string{"repository_id", "repo_full_name", "clone_url", "base_branch", "work_branch"} {
		if value := stringFromMap(lease.Metadata, metaKey); value != "" {
			entry[metaKey] = value
		}
	}
	entries[key] = entry
	return entries
}

func ensureLeaseMetadata(lease *agentcore.WorkspaceLease) map[string]interface{} {
	if lease.Metadata == nil {
		lease.Metadata = map[string]interface{}{}
	}
	return lease.Metadata
}

func markPrimaryRepositoryWorkspace(run *agentcore.AgentRun, req tools.CheckoutRepositoryRequest, lease *agentcore.WorkspaceLease, alias string) {
	if run == nil || lease == nil {
		return
	}
	leaseMetadata := ensureLeaseMetadata(lease)
	leaseMetadata["workspace_mode"] = workspace.ModeRepository
	leaseMetadata["repo_alias"] = alias
	if run.Input.Metadata == nil {
		run.Input.Metadata = map[string]interface{}{}
	}
	run.Input.Metadata["workspace_mode"] = workspace.ModeRepository
	for key, value := range map[string]string{
		"repository_id":  req.RepositoryID,
		"repo_full_name": req.RepoFullName,
		"base_branch":    req.BaseBranch,
		"work_branch":    req.WorkBranch,
		"repo_alias":     alias,
	} {
		if value = strings.TrimSpace(value); value != "" {
			run.Input.Metadata[key] = value
		}
	}
}

func copyStringAnyMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func stringFromMap(values map[string]interface{}, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (e *Engine) ensureWorkspace(ctx context.Context, agent *agentcore.Agent, run *agentcore.AgentRun, targetContext *host.TargetContext) (*agentcore.WorkspaceLease, error) {
	requiresCoding := tools.RequiresCoding(tools.AllowedSet(agent, run.Input.AllowedTools))
	if run.WorkspaceLease != nil && requiresCoding {
		info, err := os.Stat(run.WorkspaceLease.RootPath)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("the workspace for this run is unavailable; start a new run and review previously completed changes before retrying")
		}
	}
	mode := runWorkspaceMode(run)
	if mode == "" {
		mode = workspace.WorkspaceMode(agent)
	}
	if (mode == "" || mode == "analysis") && tools.AllowedSet(agent, run.Input.AllowedTools)["run_python"] {
		if run.WorkspaceLease != nil {
			return run.WorkspaceLease, nil
		}
		lease, err := workspace.NewScratch(run.AppID, run.ID)
		if err != nil {
			return nil, err
		}
		run.WorkspaceLease = lease
		if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
			return nil, err
		}
		return lease, nil
	}
	if mode == "" {
		return nil, nil
	}
	if mode != workspace.ModeHostPrepared && mode != workspace.ModeRepository {
		return nil, fmt.Errorf("unsupported workspace mode %q", mode)
	}
	if run.WorkspaceLease != nil && mode == workspace.ModeHostPrepared {
		workspace.NormalizeLease(run.WorkspaceLease)
		return run.WorkspaceLease, nil
	}
	if e.cfg.Workspaces == nil {
		return nil, fmt.Errorf("%s workspace requested but workspace registry is not configured", mode)
	}
	provider, ok := e.cfg.Workspaces.Provider(run.AppID)
	if !ok {
		return nil, fmt.Errorf("%s workspace requested but no workspace provider is configured for app %q", mode, run.AppID)
	}
	if run.WorkspaceLease != nil && mode == workspace.ModeRepository {
		workspace.NormalizeLease(run.WorkspaceLease)
		if validator, ok := provider.(workspace.LeaseValidator); ok {
			lease, valid, err := validator.ValidateWorkspace(ctx, workspace.PrepareRequest{
				AppID:           run.AppID,
				RunID:           run.ID,
				AgentID:         run.AgentID,
				RuntimeKind:     run.RuntimeKind,
				Target:          run.Target,
				TargetContext:   targetContext,
				Instructions:    run.Input.Instructions,
				Trigger:         run.Input.Trigger,
				Metadata:        run.Input.Metadata,
				WorkspaceMode:   mode,
				ExecutionConfig: agent.ExecutionConfig,
			}, *run.WorkspaceLease)
			if err != nil {
				return nil, err
			}
			if valid && lease != nil {
				run.WorkspaceLease = lease
				if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
					return nil, err
				}
				return run.WorkspaceLease, nil
			}
		} else {
			return run.WorkspaceLease, nil
		}
		if requiresCoding {
			return nil, fmt.Errorf("the workspace for this run is unavailable; start a new run and review previously completed changes before retrying")
		}
		// Read-only runs can resume on another worker by preparing a new checkout.
		// Coding runs must preserve their original checkout and any local changes.
	}
	lease, err := provider.PrepareWorkspace(ctx, workspace.PrepareRequest{
		AppID:           run.AppID,
		RunID:           run.ID,
		AgentID:         run.AgentID,
		RuntimeKind:     run.RuntimeKind,
		Target:          run.Target,
		TargetContext:   targetContext,
		Instructions:    run.Input.Instructions,
		Trigger:         run.Input.Trigger,
		Metadata:        run.Input.Metadata,
		WorkspaceMode:   mode,
		ExecutionConfig: agent.ExecutionConfig,
	})
	if err != nil {
		return nil, err
	}
	workspace.NormalizeLease(lease)
	run.WorkspaceLease = lease
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		return nil, err
	}
	e.emitRunEvent(ctx, run, "workspace.prepared", map[string]interface{}{"lease_id": lease.ID, "provider": lease.Provider, "metadata": lease.Metadata})
	return lease, nil
}

func runWorkspaceMode(run *agentcore.AgentRun) string {
	if run == nil {
		return ""
	}
	mode := strings.TrimSpace(firstMapString(run.Input.Metadata, "workspace_mode"))
	if mode != "" {
		return mode
	}
	if raw, ok := run.Input.Metadata["workspace"].(map[string]interface{}); ok {
		if mode = strings.TrimSpace(firstMapString(raw, "mode")); mode != "" {
			return mode
		}
	}
	if run.WorkspaceLease != nil {
		if mode = strings.TrimSpace(firstMapString(run.WorkspaceLease.Metadata, "workspace_mode")); mode != "" {
			return mode
		}
		// Repository leases created before dynamic checkouts recorded their mode
		// can still be reattached after a pause or a worker change.
		if strings.TrimSpace(run.WorkspaceLease.Provider) == "repository" {
			return workspace.ModeRepository
		}
	}
	return ""
}

func (e *Engine) finalizeWorkspace(ctx context.Context, run *agentcore.AgentRun, lease *agentcore.WorkspaceLease, outcome, errorMessage string, outputSummary json.RawMessage) error {
	if lease == nil || lease.Provider == "analysis" || e.cfg.Workspaces == nil {
		return nil
	}
	provider, ok := e.cfg.Workspaces.Provider(run.AppID)
	if !ok {
		return nil
	}
	result, err := provider.FinalizeWorkspace(ctx, workspace.FinalizeRequest{
		AppID:         run.AppID,
		RunID:         run.ID,
		AgentID:       run.AgentID,
		RuntimeKind:   run.RuntimeKind,
		Target:        run.Target,
		Lease:         *lease,
		Outcome:       outcome,
		ErrorMessage:  errorMessage,
		OutputSummary: outputSummary,
	})
	if err != nil {
		return err
	}
	if result != nil && len(result.OutputSummary) > 0 {
		run.OutputSummary = mergeOutputSummaries(outputSummary, result.OutputSummary)
	}
	e.emitRunEvent(ctx, run, "workspace.finalized", map[string]interface{}{"lease_id": lease.ID, "outcome": outcome})
	return nil
}

func (e *Engine) cleanupWorkspace(ctx context.Context, run *agentcore.AgentRun, reason string, terminal bool) {
	if err := e.cleanupWorkspaceOnce(ctx, run, reason, terminal); err != nil {
		appID, runID := "", ""
		if run != nil {
			appID, runID = run.AppID, run.ID
		}
		slog.ErrorContext(ctx, "workspace cleanup failed", "app_id", appID, "run_id", runID, "reason", reason, "error", err)
	}
}

func (e *Engine) cleanupWorkspaceOnce(ctx context.Context, run *agentcore.AgentRun, reason string, terminal bool) error {
	if run == nil || run.WorkspaceLease == nil {
		return nil
	}
	var ephemeralErr error
	if terminal {
		ephemeralErr = workspace.CleanupToolState(run.WorkspaceLease.RootPath)
	}
	if run != nil && run.WorkspaceLease != nil && run.WorkspaceLease.Provider == "analysis" {
		if !terminal {
			return nil
		}
		if err := workspace.CleanupScratch(run.AppID, run.ID); err != nil {
			return errors.Join(ephemeralErr, err)
		}
		return ephemeralErr
	}
	if e.cfg.Workspaces == nil {
		return ephemeralErr
	}
	if !workspace.ShouldCleanup(run.WorkspaceLease, terminal) {
		return ephemeralErr
	}
	provider, ok := e.cfg.Workspaces.Provider(run.AppID)
	if !ok {
		return ephemeralErr
	}
	if err := provider.CleanupWorkspace(ctx, workspace.CleanupRequest{
		AppID:       run.AppID,
		RunID:       run.ID,
		AgentID:     run.AgentID,
		RuntimeKind: run.RuntimeKind,
		Target:      run.Target,
		Lease:       *run.WorkspaceLease,
		Reason:      reason,
	}); err != nil {
		return errors.Join(ephemeralErr, err)
	}
	e.emitRunEvent(ctx, run, "workspace.cleaned", map[string]interface{}{"lease_id": run.WorkspaceLease.ID, "reason": reason})
	return ephemeralErr
}

func (e *Engine) failRun(ctx context.Context, run *agentcore.AgentRun, message string) {
	if run == nil {
		return
	}
	if stored, terminal, err := e.currentTerminalRun(ctx, run); err == nil && terminal {
		*run = *stored
		e.clearRunCredentials(ctx, run)
		return
	}
	now := time.Now().UTC()
	run.Status = agentcore.RunStatusFailed
	run.PauseReason = agentcore.PauseReasonNone
	run.ErrorMessage = strings.TrimSpace(message)
	run.CompletedAt = &now
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		slog.Error("persist failed run state", "app_id", run.AppID, "run_id", run.ID, "error", err)
	} else {
		e.clearRunCredentials(ctx, run)
	}
	e.emitRunEvent(ctx, run, "run.failed", e.terminalEventData(run, map[string]interface{}{"error": run.ErrorMessage}))
}

func (e *Engine) clearRunCredentials(ctx context.Context, run *agentcore.AgentRun) {
	if e == nil || e.cfg.Store == nil || run == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if store, ok := e.cfg.Store.(agentcore.ModelCredentialStore); ok {
		if err := store.ClearRunModelCredential(cleanupCtx, run.AppID, run.ID); err != nil {
			slog.Error("clear terminal model credential failed", "run_id", run.ID)
		}
	}
	if err := e.cfg.Store.ClearRunMCPCredentials(cleanupCtx, run.AppID, run.ID); err != nil {
		slog.Error("clear terminal run MCP credentials failed", "app_id", run.AppID, "run_id", run.ID, "error", err)
	}
}

func (e *Engine) requireRun(ctx context.Context, appID, runID string) (*agentcore.AgentRun, error) {
	if e == nil || e.cfg.Store == nil {
		return nil, fmt.Errorf("engine store is not configured")
	}
	run, err := e.cfg.Store.GetRun(ctx, strings.TrimSpace(appID), strings.TrimSpace(runID))
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("run not found")
	}
	return run, nil
}

func (e *Engine) resolveTarget(ctx context.Context, appID string, target agentcore.TargetRef) (*host.TargetContext, error) {
	return e.resolveTargetForRun(ctx, host.TargetContextRequest{AppID: appID, Target: target})
}

func (e *Engine) resolveTargetForRun(ctx context.Context, req host.TargetContextRequest) (*host.TargetContext, error) {
	if e.cfg.Targets == nil {
		return (&host.StaticContextProvider{}).ResolveTarget(ctx, req.AppID, req.Target)
	}
	if resolver, ok := e.cfg.Targets.(host.RunTargetContextProvider); ok {
		return resolver.ResolveRunTarget(ctx, req)
	}
	return e.cfg.Targets.ResolveTarget(ctx, req.AppID, req.Target)
}

func (e *Engine) emit(ctx context.Context, event Event) {
	if e != nil && e.cfg.EventSink != nil {
		e.cfg.EventSink.Emit(ctx, event)
	}
}

func (e *Engine) emitRunEvent(ctx context.Context, run *agentcore.AgentRun, eventType string, data map[string]interface{}) {
	if run == nil {
		return
	}
	if agentcore.RequiresExplicitTurnFinish(run) && strings.HasPrefix(eventType, "run.") {
		slog.InfoContext(ctx, "turn lifecycle", "run_id", run.ID, "turn_id", agentcore.TurnIdentity(run), "event_type", eventType, "runtime_revision", agentcore.RuntimeBuildRevision())
	}
	e.emit(ctx, Event{
		AppID:     run.AppID,
		RunID:     run.ID,
		HostRunID: run.HostRunID,
		Type:      eventType,
		Data:      withHostRunID(agentcore.WithTurnEventMetadata(run, eventType, data), run.HostRunID),
	})
}

func withHostRunID(data map[string]interface{}, hostRunID string) map[string]interface{} {
	if data != nil {
		cp := map[string]interface{}{}
		for key, value := range data {
			cp[key] = value
		}
		data = cp
	}
	hostRunID = strings.TrimSpace(hostRunID)
	if hostRunID == "" {
		return data
	}
	if data == nil {
		data = map[string]interface{}{}
	}
	if _, ok := data["host_run_id"]; !ok {
		data["host_run_id"] = hostRunID
	}
	return data
}

func (e *Engine) emitUsageCheckpoint(ctx context.Context, run *agentcore.AgentRun, summary json.RawMessage) {
	usage := usageFromSummary(summary)
	if usage.TotalTokens == 0 && usage.InputTokens == 0 && usage.CachedInputTokens == 0 && usage.OutputTokens == 0 && usage.ReasoningOutputTokens == 0 {
		return
	}
	e.emitRunEvent(ctx, run, "usage.checkpoint", map[string]interface{}{
		"usage":          usage,
		"usage_semantic": "cumulative",
	})
}

func (e *Engine) terminalEventData(run *agentcore.AgentRun, data map[string]interface{}) map[string]interface{} {
	if data == nil {
		data = map[string]interface{}{}
	} else {
		cp := map[string]interface{}{}
		for key, value := range data {
			cp[key] = value
		}
		data = cp
	}
	if run != nil {
		var summary map[string]json.RawMessage
		if json.Unmarshal(run.OutputSummary, &summary) == nil && len(summary["interrupted_external_effects"]) > 0 {
			data["interrupted_external_effects"] = summary["interrupted_external_effects"]
		}
		usage := usageFromSummary(run.OutputSummary)
		if usage.TotalTokens != 0 || usage.InputTokens != 0 || usage.CachedInputTokens != 0 || usage.OutputTokens != 0 || usage.ReasoningOutputTokens != 0 {
			data["usage"] = usage
			data["usage_semantic"] = "cumulative"
		}
	}
	return data
}

func usageFromSummary(summary json.RawMessage) agentcore.Usage {
	var usage agentcore.Usage
	if len(summary) == 0 {
		return usage
	}
	var body map[string]interface{}
	if err := json.Unmarshal(summary, &body); err != nil {
		return usage
	}
	usage.InputTokens = int64FromAny(body["input_tokens"])
	usage.CachedInputTokens = int64FromAny(body["cached_input_tokens"])
	usage.OutputTokens = int64FromAny(body["output_tokens"])
	usage.ReasoningOutputTokens = int64FromAny(body["reasoning_output_tokens"])
	usage.TotalTokens = int64FromAny(body["total_tokens"])
	if usage.TotalTokens == 0 {
		// Cached input is a subset of input tokens and reasoning output is a
		// subset of output tokens. They are details, not additional usage.
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage
}

func cumulativeOutputSummary(base, current json.RawMessage, runtimeKind string) json.RawMessage {
	if len(current) == 0 {
		return current
	}
	if runtimeKind != agentcore.RuntimeNativeSDK {
		return current
	}
	var semantic struct {
		UsageSemantic string `json:"usage_semantic"`
	}
	if json.Unmarshal(current, &semantic) == nil && semantic.UsageSemantic == "cumulative" {
		return current
	}
	baseUsage := usageFromSummary(base)
	currentUsage := usageFromSummary(current)
	if currentUsage.TotalTokens == 0 && currentUsage.InputTokens == 0 && currentUsage.CachedInputTokens == 0 && currentUsage.OutputTokens == 0 && currentUsage.ReasoningOutputTokens == 0 {
		return current
	}
	if baseUsage.TotalTokens == 0 && baseUsage.InputTokens == 0 && baseUsage.CachedInputTokens == 0 && baseUsage.OutputTokens == 0 && baseUsage.ReasoningOutputTokens == 0 {
		return current
	}
	usage := agentcore.Usage{
		InputTokens:           baseUsage.InputTokens + currentUsage.InputTokens,
		CachedInputTokens:     baseUsage.CachedInputTokens + currentUsage.CachedInputTokens,
		OutputTokens:          baseUsage.OutputTokens + currentUsage.OutputTokens,
		ReasoningOutputTokens: baseUsage.ReasoningOutputTokens + currentUsage.ReasoningOutputTokens,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	return outputSummaryWithUsage(current, usage)
}

func outputSummaryWithUsage(summary json.RawMessage, usage agentcore.Usage) json.RawMessage {
	var body map[string]interface{}
	if err := json.Unmarshal(summary, &body); err != nil {
		return summary
	}
	body["total_tokens"] = usage.TotalTokens
	body["input_tokens"] = usage.InputTokens
	body["cached_input_tokens"] = usage.CachedInputTokens
	body["output_tokens"] = usage.OutputTokens
	body["reasoning_output_tokens"] = usage.ReasoningOutputTokens
	out, err := json.Marshal(body)
	if err != nil {
		return summary
	}
	return out
}

func mergeOutputSummaries(base, overlay json.RawMessage) json.RawMessage {
	if len(overlay) == 0 {
		return base
	}
	if len(base) == 0 {
		return overlay
	}
	var baseMap map[string]interface{}
	var overlayMap map[string]interface{}
	if err := json.Unmarshal(base, &baseMap); err != nil {
		return overlay
	}
	if err := json.Unmarshal(overlay, &overlayMap); err != nil {
		return overlay
	}
	for key, value := range overlayMap {
		baseMap[key] = value
	}
	merged, err := json.Marshal(baseMap)
	if err != nil {
		return overlay
	}
	return merged
}

func int64FromAny(value interface{}) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		out, _ := typed.Int64()
		return out
	default:
		return 0
	}
}

func normalizeTools(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = canonicalConfiguredToolName(tools.CanonicalName(value))
		if value != "" && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

// canonicalConfiguredToolName folds names retired by tool consolidations onto
// their replacements, so an agent configured before a consolidation keeps the
// equivalent capability instead of silently losing a tool. Note the mapping only
// carries a tool forward when the old name has a successor: a config listing
// read_file gains read_files, but nothing maps into read_symbol or trace_symbol
// unless it already listed find_symbol or find_callers/find_callees.
// Entries are permanent — removing one strands every config that still uses it.
func canonicalConfiguredToolName(name string) string {
	switch strings.TrimSpace(name) {
	case "checkout_repository":
		return "checkout_repositories"
	case "read_file", "read_file_range":
		return "read_files"
	case "search_files", "ripgrep", "grep":
		return "repository_search"
	case "find_symbol":
		return "read_symbol"
	case "find_callers", "find_callees":
		return "trace_symbol"
	case "list_available_skills", "search_available_skills":
		return "find_skills"
	case "web_search_brave", "web_search_exa":
		return "web_search"
	default:
		return strings.TrimSpace(name)
	}
}

type artifactWriter struct {
	store agentcore.Store
	run   *agentcore.AgentRun
}

func (w artifactWriter) WriteArtifact(ctx context.Context, artifact agentcore.AgentRunArtifact) error {
	artifact.AppID = w.run.AppID
	artifact.RunID = w.run.ID
	return w.store.AppendArtifact(ctx, &artifact)
}

type interactionBroker struct {
	store agentcore.Store
	run   *agentcore.AgentRun
}

func (b interactionBroker) RequestInteraction(ctx context.Context, interaction agentcore.AgentRunInteraction) error {
	interaction.AppID = b.run.AppID
	interaction.RunID = b.run.ID
	interaction.RuntimeKind = b.run.RuntimeKind
	return b.store.AppendInteraction(ctx, &interaction)
}

type runtimeEventSink struct {
	sink      EventSink
	hostRunID string
}

func (s runtimeEventSink) Emit(ctx context.Context, event runtime.Event) {
	if s.sink == nil {
		return
	}
	s.sink.Emit(ctx, Event{
		AppID:     event.AppID,
		RunID:     event.RunID,
		HostRunID: s.hostRunID,
		Type:      event.Type,
		Data:      withHostRunID(event.Data, s.hostRunID),
	})
}
