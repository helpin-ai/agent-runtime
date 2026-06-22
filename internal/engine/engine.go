package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/id"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

const (
	ExecutionModeDurable     = "durable"
	ExecutionModeLightweight = "lightweight"
)

type Config struct {
	DefaultExecutionMode string
	Store                agentcore.Store
	Runtimes             *runtime.Registry
	Tools                *tools.Registry
	Targets              host.TargetContextProvider
	Skills               *skills.Registry
	SkillPackages        *skills.PackageStoreRegistry
	Workspaces           *workspace.Registry
	Durable              DurableExecutor
	EventSink            EventSink
}

type Engine struct {
	cfg Config
}

type DurableExecutor interface {
	StartRun(ctx context.Context, run *agentcore.AgentRun) error
	CancelRun(ctx context.Context, run *agentcore.AgentRun) error
	ResumeRun(ctx context.Context, run *agentcore.AgentRun, payload ResumePayload) error
}

type EventSink interface {
	Emit(ctx context.Context, event Event)
}

type Event struct {
	AppID string                 `json:"app_id"`
	RunID string                 `json:"run_id"`
	Type  string                 `json:"type"`
	Data  map[string]interface{} `json:"data,omitempty"`
}

type SlogEventSink struct{}

func (SlogEventSink) Emit(_ context.Context, event Event) {
	slog.Info("agent runtime event", "app_id", event.AppID, "run_id", event.RunID, "type", event.Type)
}

type StartRunRequest struct {
	AppID           string                 `json:"app_id"`
	AgentID         string                 `json:"agent_id"`
	Target          agentcore.TargetRef    `json:"target"`
	Instructions    string                 `json:"instructions,omitempty"`
	AllowedTools    []string               `json:"allowed_tools,omitempty"`
	ExternalActorID string                 `json:"external_actor_id,omitempty"`
	Mode            string                 `json:"mode,omitempty"`
	ExecutionMode   string                 `json:"execution_mode,omitempty"`
	Trigger         map[string]interface{} `json:"trigger,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
}

type ResumePayload struct {
	Intent          string          `json:"intent"`
	Content         string          `json:"content,omitempty"`
	ResponsePayload json.RawMessage `json:"response_payload,omitempty"`
	ExternalActorID string          `json:"external_actor_id,omitempty"`
}

func New(cfg Config) *Engine {
	if cfg.DefaultExecutionMode == "" {
		cfg.DefaultExecutionMode = ExecutionModeLightweight
	}
	return &Engine{cfg: cfg}
}

func (e *Engine) StartRun(ctx context.Context, req StartRunRequest) (*agentcore.AgentRun, error) {
	if e == nil || e.cfg.Store == nil {
		return nil, fmt.Errorf("engine store is not configured")
	}
	req.AppID = strings.TrimSpace(req.AppID)
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.Target.Type = strings.TrimSpace(req.Target.Type)
	req.Target.ID = strings.TrimSpace(req.Target.ID)
	if req.AppID == "" || req.AgentID == "" {
		return nil, fmt.Errorf("app_id and agent_id are required")
	}
	if req.Target.Type == "" || req.Target.ID == "" {
		return nil, fmt.Errorf("target.type and target.id are required")
	}
	agent, err := e.cfg.Store.GetAgent(ctx, req.AppID, req.AgentID)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, fmt.Errorf("agent not found")
	}
	if len(agent.AllowedTargets) > 0 && !slices.Contains(agent.AllowedTargets, req.Target.Type) {
		return nil, fmt.Errorf("target type %q is not allowed for agent", req.Target.Type)
	}
	if err := tools.ValidateAllowedSubset(agent, req.AllowedTools); err != nil {
		return nil, err
	}

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
			Instructions:   strings.TrimSpace(req.Instructions),
			AllowedTools:   normalizeTools(req.AllowedTools),
			Trigger:        req.Trigger,
			Metadata:       req.Metadata,
			ContextSummary: targetContext.Summary,
		},
		OutputSummary: json.RawMessage(`{}`),
	}
	if err := e.cfg.Store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "run.queued"})

	switch mode {
	case ExecutionModeLightweight:
		go e.executeLightweight(context.Background(), run.AppID, run.ID)
	case ExecutionModeDurable:
		if e.cfg.Durable == nil {
			return nil, fmt.Errorf("durable execution requested but durable executor is not configured")
		}
		if err := e.cfg.Durable.StartRun(ctx, run); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported execution_mode %q", mode)
	}
	return run, nil
}

func (e *Engine) CancelRun(ctx context.Context, appID, runID string) (*agentcore.AgentRun, error) {
	run, err := e.requireRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if agentcore.IsTerminalStatus(run.Status) {
		return run, nil
	}
	if run.ExecutionMode == ExecutionModeDurable && e.cfg.Durable != nil {
		if err := e.cfg.Durable.CancelRun(ctx, run); err != nil {
			return nil, err
		}
	}
	e.cleanupWorkspace(ctx, run, "cancelled", true)
	now := time.Now().UTC()
	run.Status = agentcore.RunStatusCancelled
	run.PauseReason = agentcore.PauseReasonNone
	run.CompletedAt = &now
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		return nil, err
	}
	e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "run.cancelled"})
	return run, nil
}

func (e *Engine) ResumeRun(ctx context.Context, appID, runID string, payload ResumePayload) (*agentcore.AgentRun, error) {
	run, err := e.requireRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if agentcore.IsTerminalStatus(run.Status) {
		return nil, fmt.Errorf("run is terminal")
	}
	if run.ExecutionMode == ExecutionModeDurable && e.cfg.Durable != nil {
		if err := e.cfg.Durable.ResumeRun(ctx, run, payload); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(payload.Content) != "" {
		_ = e.cfg.Store.AppendMessage(ctx, &agentcore.AgentRunMessage{
			AppID:       run.AppID,
			RunID:       run.ID,
			Role:        "user",
			Content:     strings.TrimSpace(payload.Content),
			MessageType: "message",
		})
	}
	e.resolvePendingInteraction(ctx, run, payload)
	if run.Input.Metadata == nil {
		run.Input.Metadata = map[string]interface{}{}
	}
	lastResume := map[string]interface{}{
		"intent":            strings.TrimSpace(payload.Intent),
		"content":           strings.TrimSpace(payload.Content),
		"external_actor_id": strings.TrimSpace(payload.ExternalActorID),
	}
	if len(payload.ResponsePayload) > 0 {
		lastResume["response_payload"] = json.RawMessage(append(json.RawMessage(nil), payload.ResponsePayload...))
	}
	run.Input.Metadata["last_resume"] = lastResume
	run.Status = agentcore.RunStatusRunning
	run.PauseReason = agentcore.PauseReasonNone
	if payload.Intent == "approve" {
		run.ApprovalState = agentcore.ApprovalApproved
	}
	if payload.Intent == "request_changes" {
		run.ApprovalState = agentcore.ApprovalRejected
	}
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		return nil, err
	}
	e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "run.resumed"})
	if run.ExecutionMode == ExecutionModeLightweight {
		go e.executeLightweight(context.Background(), run.AppID, run.ID)
	}
	return run, nil
}

func (e *Engine) resolvePendingInteraction(ctx context.Context, run *agentcore.AgentRun, payload ResumePayload) {
	if e == nil || e.cfg.Store == nil || run == nil {
		return
	}
	interactions, err := e.cfg.Store.ListInteractions(ctx, run.AppID, run.ID)
	if err != nil {
		return
	}
	for i := len(interactions) - 1; i >= 0; i-- {
		interaction := interactions[i]
		if strings.TrimSpace(interaction.Status) != "pending" {
			continue
		}
		interaction.Status = "resolved"
		interaction.ResolvedByExternalID = strings.TrimSpace(payload.ExternalActorID)
		resolvedAt := time.Now().UTC()
		interaction.ResolvedAt = &resolvedAt
		interaction.ResponsePayload = resumeInteractionResponsePayload(payload)
		_ = e.cfg.Store.UpdateInteraction(ctx, &interaction)
		return
	}
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

func (e *Engine) executeLightweight(ctx context.Context, appID, runID string) {
	if _, err := e.ExecuteRunOnce(ctx, appID, runID); err != nil {
		slog.Error("lightweight run execution failed", "app_id", appID, "run_id", runID, "error", err)
	}
}

func (e *Engine) resolveSkills(ctx context.Context, agent *agentcore.Agent, run *agentcore.AgentRun) (skills.Resolution, error) {
	if agent == nil || len(agent.Skills) == 0 {
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
	resolution, err := e.cfg.Skills.ResolveForContext(ctx, lookupCtx, agent.Skills)
	if err != nil {
		return skills.Resolution{}, err
	}
	if agent.RuntimeKind == agentcore.RuntimeNativeSDK {
		resolution = skills.SelectNativeActiveResolution(resolution, nativeActiveSelectionContext(agent, run))
	}
	allowedTools := agent.AllowedTools
	if run != nil && len(run.Input.AllowedTools) > 0 {
		allowedTools = run.Input.AllowedTools
	}
	if err := skills.ValidateRuntimeAndTools(agent.RuntimeKind, allowedTools, resolution.Definitions); err != nil {
		return skills.Resolution{}, err
	}
	if len(resolution.CoreRefs) > 0 {
		agent.Skills = resolution.CoreRefs
	}
	return resolution, nil
}

func nativeActiveSelectionContext(agent *agentcore.Agent, run *agentcore.AgentRun) skills.NativeActiveSelectionContext {
	var ctx skills.NativeActiveSelectionContext
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

func (e *Engine) stageRuntimeSkills(ctx context.Context, agent *agentcore.Agent, run *agentcore.AgentRun, resolution skills.Resolution, lease *agentcore.WorkspaceLease, targetContext *host.TargetContext) (string, error) {
	if len(resolution.CoreRefs) == 0 || len(resolution.Definitions) == 0 {
		return "", nil
	}
	if lease == nil || strings.TrimSpace(lease.RootPath) == "" {
		return "", nil
	}
	stageRoot := filepath.Join(strings.TrimSpace(lease.RootPath), ".agent-runtime", "skills")
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
	if err := skills.StageResolvedInto(ctx, resolution, skills.StageOptions{
		Lookup:        lookup,
		PackageStore:  packageStore,
		LookupContext: lookupCtx,
		DestRoot:      stageRoot,
	}); err != nil {
		return "", fmt.Errorf("stage runtime skills: %w", err)
	}
	if err := e.persistRuntimeSkillManifest(ctx, run, stageRoot, resolution); err != nil {
		return "", err
	}
	if targetContext != nil && targetContext.Data != nil {
		targetContext.Data["staged_skill_root"] = stageRoot
	}
	return stageRoot, nil
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
	entries := make([]manifestEntry, 0, len(resolution.CoreRefs))
	for idx, ref := range resolution.CoreRefs {
		definition := skills.Definition{}
		if idx < len(resolution.Definitions) {
			definition = resolution.Definitions[idx]
		}
		entries = append(entries, manifestEntry{
			Key:        definition.Key,
			SourceKind: definition.SourceKind,
			VersionKey: ref.VersionKey,
			SkillID:    ref.SkillID,
		})
	}
	payload, err := json.Marshal(map[string]interface{}{
		"runtime_kind": run.RuntimeKind,
		"staged_root":  stageRoot,
		"skills":       entries,
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
	agent, err := e.cfg.Store.GetAgent(ctx, run.AppID, run.AgentID)
	if err != nil || agent == nil {
		e.failRun(ctx, run, "agent not found")
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("agent not found")
	}
	if run.ApprovalState == agentcore.ApprovalPending {
		run.Status = agentcore.RunStatusPaused
		run.PauseReason = agentcore.PauseReasonHumanApproval
		_ = e.cfg.Store.UpdateRun(ctx, run)
		e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "run.paused", Data: map[string]interface{}{"pause_reason": run.PauseReason}})
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
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "run.started"})
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
	workspaceLease, err := e.ensureWorkspace(ctx, agent, run, targetContext)
	if err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	stagedSkillRoot, err := e.stageRuntimeSkills(ctx, agent, run, skillResolution, workspaceLease, targetContext)
	if err != nil {
		e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusFailed, err.Error(), nil)
		e.cleanupWorkspace(ctx, run, "failed", true)
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	result, err := adapter.Execute(&runtime.ExecutionContext{
		Context:           ctx,
		AppID:             run.AppID,
		Agent:             agent,
		Run:               run,
		Store:             e.cfg.Store,
		TargetContext:     targetContext,
		WorkspaceLease:    workspaceLease,
		AllowedTools:      tools.AllowedSet(agent, run.Input.AllowedTools),
		Tools:             e.cfg.Tools,
		SkillRefs:         skillResolution.CoreRefs,
		SkillDefinitions:  skillResolution.Definitions,
		SkillInstructions: skillResolution.Instructions,
		SkillPolicy:       skillResolution.Policy,
		StagedSkillRoot:   stagedSkillRoot,
		ArtifactWriter:    artifactWriter{store: e.cfg.Store, run: run},
		InteractionBroker: interactionBroker{store: e.cfg.Store, run: run},
		EventSink:         runtimeEventSink{sink: e.cfg.EventSink},
	})
	if err != nil {
		e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusFailed, err.Error(), nil)
		e.cleanupWorkspace(ctx, run, "failed", true)
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	if result == nil {
		result = &runtime.Result{}
	}
	if result.AssistantMessage != "" && !result.MessagesPersisted {
		_ = e.cfg.Store.AppendMessage(ctx, &agentcore.AgentRunMessage{
			AppID:       run.AppID,
			RunID:       run.ID,
			Role:        "assistant",
			Content:     result.AssistantMessage,
			MessageType: "message",
		})
	}
	if result.WaitForApproval || result.AwaitingInput || result.AwaitingAuth {
		if err := e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusPaused, "", result.OutputSummary); err != nil {
			e.failRun(ctx, run, err.Error())
			return nil, err
		}
		if len(result.OutputSummary) > 0 {
			run.OutputSummary = result.OutputSummary
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
		e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "run.paused", Data: map[string]interface{}{"pause_reason": run.PauseReason}})
		return result, nil
	}
	completedAt := time.Now().UTC()
	if len(result.OutputSummary) > 0 {
		run.OutputSummary = result.OutputSummary
	}
	if err := e.finalizeWorkspace(ctx, run, workspaceLease, agentcore.RunStatusCompleted, "", result.OutputSummary); err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	run.Status = agentcore.RunStatusCompleted
	run.PauseReason = agentcore.PauseReasonNone
	run.CompletedAt = &completedAt
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		e.failRun(ctx, run, err.Error())
		return nil, err
	}
	e.cleanupWorkspace(ctx, run, "completed", true)
	e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "run.completed"})
	return result, nil
}

func (e *Engine) ensureWorkspace(ctx context.Context, agent *agentcore.Agent, run *agentcore.AgentRun, targetContext *host.TargetContext) (*agentcore.WorkspaceLease, error) {
	if !workspace.RequiresHostPrepared(agent) {
		return nil, nil
	}
	if run.WorkspaceLease != nil {
		workspace.NormalizeLease(run.WorkspaceLease)
		return run.WorkspaceLease, nil
	}
	if e.cfg.Workspaces == nil {
		return nil, fmt.Errorf("host-prepared workspace requested but workspace registry is not configured")
	}
	provider, ok := e.cfg.Workspaces.Provider(run.AppID)
	if !ok {
		return nil, fmt.Errorf("host-prepared workspace requested but no workspace provider is configured for app %q", run.AppID)
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
		WorkspaceMode:   workspace.ModeHostPrepared,
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
	e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "workspace.prepared", Data: map[string]interface{}{"lease_id": lease.ID, "provider": lease.Provider}})
	return lease, nil
}

func (e *Engine) finalizeWorkspace(ctx context.Context, run *agentcore.AgentRun, lease *agentcore.WorkspaceLease, outcome, errorMessage string, outputSummary json.RawMessage) error {
	if lease == nil || e.cfg.Workspaces == nil {
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
		run.OutputSummary = result.OutputSummary
	}
	e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "workspace.finalized", Data: map[string]interface{}{"lease_id": lease.ID, "outcome": outcome}})
	return nil
}

func (e *Engine) cleanupWorkspace(ctx context.Context, run *agentcore.AgentRun, reason string, terminal bool) {
	if run == nil || run.WorkspaceLease == nil || e.cfg.Workspaces == nil {
		return
	}
	if !workspace.ShouldCleanup(run.WorkspaceLease, terminal) {
		return
	}
	provider, ok := e.cfg.Workspaces.Provider(run.AppID)
	if !ok {
		return
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
		slog.Error("workspace cleanup failed", "app_id", run.AppID, "run_id", run.ID, "reason", reason, "error", err)
		return
	}
	e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "workspace.cleaned", Data: map[string]interface{}{"lease_id": run.WorkspaceLease.ID, "reason": reason}})
}

func (e *Engine) failRun(ctx context.Context, run *agentcore.AgentRun, message string) {
	if run == nil {
		return
	}
	now := time.Now().UTC()
	run.Status = agentcore.RunStatusFailed
	run.PauseReason = agentcore.PauseReasonNone
	run.ErrorMessage = strings.TrimSpace(message)
	run.CompletedAt = &now
	_ = e.cfg.Store.UpdateRun(ctx, run)
	e.emit(ctx, Event{AppID: run.AppID, RunID: run.ID, Type: "run.failed", Data: map[string]interface{}{"error": run.ErrorMessage}})
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

func normalizeTools(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = tools.CanonicalName(value)
		if value != "" && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
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
	sink EventSink
}

func (s runtimeEventSink) Emit(ctx context.Context, event runtime.Event) {
	if s.sink == nil {
		return
	}
	s.sink.Emit(ctx, Event{
		AppID: event.AppID,
		RunID: event.RunID,
		Type:  event.Type,
		Data:  event.Data,
	})
}
