package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/id"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/skills"
)

func (e *Engine) enforceRequiredCompletionInteraction(
	ctx context.Context,
	run *agentcore.AgentRun,
	resolution skills.Resolution,
	result *runtime.Result,
) error {
	if e == nil || e.cfg.Store == nil || run == nil || result == nil {
		return nil
	}
	if result.WaitForApproval || result.AwaitingInput || result.AwaitingAuth {
		return nil
	}
	required := skills.CompletionRequiredInteractionKinds(resolution.Policy, resolution.Definitions)
	if len(required) == 0 || completionAllowedAfterApproval(run) {
		return nil
	}
	interactions, err := e.cfg.Store.ListInteractions(ctx, run.AppID, run.ID)
	if err != nil {
		return fmt.Errorf("list required completion interactions: %w", err)
	}
	for index := len(interactions) - 1; index >= 0; index-- {
		interaction := interactions[index]
		if strings.TrimSpace(interaction.Status) != "pending" {
			continue
		}
		switch normalizeCompletionInteractionKind(interaction.InteractionKind) {
		case skills.InteractionKindApprovalRequest, skills.InteractionKindReviewCheckpoint:
			result.WaitForApproval = true
			return nil
		case skills.InteractionKindRequestUserInput:
			result.AwaitingInput = true
			return nil
		}
	}

	interactionKind := preferredApprovalInteractionKind(required)
	if interactionKind == "" {
		return fmt.Errorf(
			"run completed without an interaction required by the active skills: %s",
			strings.Join(required, ", "),
		)
	}
	if err := e.appendCompletionApprovalInteraction(ctx, run, resolution, interactionKind, result.AssistantMessage); err != nil {
		return err
	}
	result.WaitForApproval = true
	slog.WarnContext(ctx, "synthesized missing required approval interaction",
		"app_id", run.AppID,
		"run_id", run.ID,
		"runtime_kind", run.RuntimeKind,
		"required_interactions", required,
	)
	return nil
}

func completionAllowedAfterApproval(run *agentcore.AgentRun) bool {
	if run == nil || run.Input.Metadata == nil {
		return false
	}
	raw, ok := run.Input.Metadata["last_resume"]
	if !ok {
		return false
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	var resume struct {
		Intent string `json:"intent"`
	}
	return json.Unmarshal(payload, &resume) == nil && strings.TrimSpace(resume.Intent) == "approve"
}

func normalizeCompletionInteractionKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case "human_approval", skills.InteractionKindApprovalRequest:
		return skills.InteractionKindApprovalRequest
	case "human_input", skills.InteractionKindRequestUserInput:
		return skills.InteractionKindRequestUserInput
	default:
		return strings.TrimSpace(kind)
	}
}

func preferredApprovalInteractionKind(required []string) string {
	if slices.Contains(required, skills.InteractionKindApprovalRequest) {
		return skills.InteractionKindApprovalRequest
	}
	if slices.Contains(required, skills.InteractionKindReviewCheckpoint) {
		return skills.InteractionKindReviewCheckpoint
	}
	return ""
}

func (e *Engine) appendCompletionApprovalInteraction(
	ctx context.Context,
	run *agentcore.AgentRun,
	resolution skills.Resolution,
	interactionKind string,
	assistantMessage string,
) error {
	phase, previewPanelKey, title := runtime.CompletionApprovalContextForSkills(resolution.Definitions, interactionKind)
	summary := completionApprovalSummary(assistantMessage)
	requestSchema := "approval_request_v1"
	if interactionKind == skills.InteractionKindReviewCheckpoint {
		requestSchema = "review_checkpoint_v1"
	}
	rawInput := map[string]any{
		"title":   title,
		"summary": summary,
	}
	payload := map[string]any{
		"schema":         "agent_runtime.v1",
		"request_schema": requestSchema,
		"title":          title,
		"summary":        summary,
		"raw_input":      rawInput,
	}
	if phase != "" {
		rawInput["phase"] = phase
		payload["phase"] = phase
	}
	if previewPanelKey != "" {
		rawInput["preview_panel_key"] = previewPanelKey
		payload["preview_panel_key"] = previewPanelKey
	}
	requestPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal required approval interaction: %w", err)
	}
	interactionID := id.New("int")
	if err := e.cfg.Store.AppendInteraction(ctx, &agentcore.AgentRunInteraction{
		ID:              interactionID,
		AppID:           run.AppID,
		RunID:           run.ID,
		RuntimeKind:     run.RuntimeKind,
		InteractionKind: interactionKind,
		Status:          "pending",
		Title:           title,
		Summary:         summary,
		RequestPayload:  requestPayload,
	}); err != nil {
		return fmt.Errorf("append required approval interaction: %w", err)
	}
	artifactInput, _ := json.Marshal(rawInput)
	metadata, _ := json.Marshal(map[string]any{
		"internal":       false,
		"interaction_id": interactionID,
		"source":         "completion_guard",
	})
	if err := e.cfg.Store.AppendArtifact(ctx, &agentcore.AgentRunArtifact{
		AppID:         run.AppID,
		RunID:         run.ID,
		ArtifactType:  "human_approval_request",
		Format:        "json",
		StorageMode:   "inline",
		InlineContent: string(artifactInput),
		Metadata:      metadata,
	}); err != nil {
		// The pending interaction is the authoritative pause contract. A
		// best-effort mirror artifact must not turn a valid approval wait into
		// a failed run.
		slog.WarnContext(ctx, "failed to mirror required approval artifact",
			"app_id", run.AppID,
			"run_id", run.ID,
			"interaction_id", interactionID,
			"error", err,
		)
	}
	return nil
}

func completionApprovalSummary(message string) string {
	summary := strings.Join(strings.Fields(message), " ")
	if summary == "" {
		return "The agent published a result that requires approval before the run can complete."
	}
	const limit = 240
	if len(summary) <= limit {
		return summary
	}
	return strings.TrimSpace(summary[:limit-3]) + "..."
}
