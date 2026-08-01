package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const (
	runtimeToolUpdatePlan              = "update_plan"
	runtimeToolRequestUserInput        = "request_user_input"
	runtimeToolRequestApproval         = "request_approval"
	runtimeToolRequestReviewCheckpoint = "request_review_checkpoint"
)

type runtimePlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

type runtimeUpdatePlanRequest struct {
	Explanation string                 `json:"explanation,omitempty"`
	Plan        []runtimePlanStep      `json:"plan"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
}

type runtimeApprovalRequest struct {
	Phase           string `json:"phase,omitempty"`
	PreviewPanelKey string `json:"preview_panel_key,omitempty"`
	Title           string `json:"title"`
	Summary         string `json:"summary,omitempty"`
}

type runtimeUserInputRequest struct {
	Questions []json.RawMessage `json:"questions"`
}

func runtimeInteractionToolDefinitions() []tools.Definition {
	approvalSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"phase":             map[string]any{"type": "string", "description": "Short workflow phase label such as prd, tasks, or task_doc."},
			"preview_panel_key": map[string]any{"type": "string", "description": "Optional preview panel key this approval request refers to."},
			"title":             map[string]any{"type": "string", "description": "User-facing title for the approval request."},
			"summary":           map[string]any{"type": "string", "description": "Optional short approval summary."},
		},
		"required":             []string{"title"},
		"additionalProperties": false,
	}
	return []tools.Definition{
		{
			Name:        runtimeToolUpdatePlan,
			Description: "Publish or update the current execution plan for this run. Use for complex, multi-step work; skip for simple direct single-tool requests.",
			Category:    "Planning",
			Mutating:    false,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"explanation": map[string]any{"type": "string", "description": "Optional brief note explaining why this plan is needed or what changed."},
					"plan": map[string]any{
						"type":        "array",
						"description": "Ordered run steps. Keep plans concise, usually 3-6 steps.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"step":   map[string]any{"type": "string", "description": "A concrete, user-readable step."},
								"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
							},
							"required":             []string{"step", "status"},
							"additionalProperties": false,
						},
					},
					"metadata": map[string]any{
						"type":                 "object",
						"description":          "Optional generic planning metadata.",
						"additionalProperties": true,
					},
				},
				"required":             []string{"plan"},
				"additionalProperties": false,
			},
		},
		{
			Name:        runtimeToolRequestUserInput,
			Description: "Present structured questions to the human and pause the run until they answer.",
			Category:    "Interaction",
			Mutating:    true,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"questions": map[string]any{
						"type":        "array",
						"description": "Questions to present to the human.",
						"minItems":    1,
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":       map[string]any{"type": "string"},
								"header":   map[string]any{"type": "string"},
								"question": map[string]any{"type": "string"},
								"isOther":  map[string]any{"type": "boolean"},
								"isSecret": map[string]any{"type": "boolean"},
								"options": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"label":       map[string]any{"type": "string"},
											"description": map[string]any{"type": "string"},
										},
										"required":             []string{"label"},
										"additionalProperties": false,
									},
								},
							},
							"required":             []string{"id", "question"},
							"additionalProperties": false,
						},
					},
				},
				"required":             []string{"questions"},
				"additionalProperties": false,
			},
		},
		{
			Name:        runtimeToolRequestApproval,
			Description: "Request inline approval or change feedback and pause the run until the human responds.",
			Category:    "Interaction",
			InputSchema: approvalSchema,
			Mutating:    true,
		},
		{
			Name:        runtimeToolRequestReviewCheckpoint,
			Description: "Request an inline review checkpoint and pause the run until approval or change feedback.",
			Category:    "Interaction",
			Mutating:    true,
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           approvalSchema["properties"],
				"required":             []string{"title"},
				"additionalProperties": true,
			},
		},
	}
}

func runtimeInteractionToolDefinition(name string) (tools.Definition, bool) {
	name = tools.CanonicalName(name)
	for _, def := range runtimeInteractionToolDefinitions() {
		if def.Name == name {
			return def, true
		}
	}
	return tools.Definition{}, false
}

func (g *Gateway) callRuntimeInteractionTool(ctx context.Context, run *agentcore.AgentRun, def tools.Definition, input json.RawMessage) (*CallResult, error) {
	if tools.CanonicalName(def.Name) == runtimeToolUpdatePlan {
		return g.callRuntimeUpdatePlan(ctx, run, input)
	}
	interaction, artifactType, pauseReason, err := runtimeInteractionFromTool(run, def.Name, input)
	if err != nil {
		resp := &CallResult{IsError: true, Content: []ContentItem{{Type: "text", Text: err.Error()}}}
		_ = g.recordToolCall(ctx, run, def.Name, input, resp, err, false, false)
		return resp, nil
	}
	if err := g.store.AppendInteraction(ctx, interaction); err != nil {
		return nil, err
	}
	if artifactType != "" {
		metadata, _ := json.Marshal(map[string]any{
			"internal":       false,
			"interaction_id": interaction.ID,
		})
		_ = g.store.AppendArtifact(ctx, &agentcore.AgentRunArtifact{
			AppID:         run.AppID,
			RunID:         run.ID,
			ArtifactType:  artifactType,
			Format:        "json",
			StorageMode:   "inline",
			InlineContent: string(input),
			Metadata:      metadata,
		})
	}
	payload, _ := json.Marshal(map[string]any{
		"status":           agentcore.RunStatusPaused,
		"pause_reason":     pauseReason,
		"interaction_id":   interaction.ID,
		"interaction_kind": interaction.InteractionKind,
	})
	resp := &CallResult{
		Content:       []ContentItem{{Type: "text", Text: string(payload)}},
		InteractionID: interaction.ID,
	}
	_ = g.recordToolCall(ctx, run, def.Name, input, resp, nil, false, false)
	return resp, nil
}

func (g *Gateway) callRuntimeUpdatePlan(ctx context.Context, run *agentcore.AgentRun, input json.RawMessage) (*CallResult, error) {
	var req runtimeUpdatePlanRequest
	if err := json.Unmarshal(input, &req); err != nil {
		return g.runtimeToolError(ctx, run, runtimeToolUpdatePlan, input, fmt.Errorf("parse update_plan input: %w", err))
	}
	if err := validateRuntimeUpdatePlan(&req); err != nil {
		return g.runtimeToolError(ctx, run, runtimeToolUpdatePlan, input, err)
	}
	content := map[string]any{
		"note":        req.Explanation,
		"explanation": req.Explanation,
		"plan":        req.Plan,
	}
	if len(req.Metadata) > 0 {
		content["metadata"] = req.Metadata
	}
	payload, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	metadata, _ := json.Marshal(map[string]any{"internal": false, "source": runtimeToolUpdatePlan})
	if err := g.store.AppendArtifact(ctx, &agentcore.AgentRunArtifact{
		AppID:         run.AppID,
		RunID:         run.ID,
		ArtifactType:  "run_plan",
		Format:        "json",
		StorageMode:   "inline",
		InlineContent: string(payload),
		Metadata:      metadata,
	}); err != nil {
		return nil, err
	}
	output, _ := json.Marshal(map[string]any{
		"status":        "updated",
		"artifact_type": "run_plan",
		"plan":          req.Plan,
		"explanation":   req.Explanation,
		"metadata":      req.Metadata,
	})
	resp := &CallResult{Content: []ContentItem{{Type: "text", Text: string(output)}}}
	_ = g.recordToolCall(ctx, run, runtimeToolUpdatePlan, input, resp, nil, false, false)
	return resp, nil
}

func (g *Gateway) runtimeToolError(ctx context.Context, run *agentcore.AgentRun, toolName string, input json.RawMessage, err error) (*CallResult, error) {
	resp := &CallResult{IsError: true, Content: []ContentItem{{Type: "text", Text: err.Error()}}}
	_ = g.recordToolCall(ctx, run, toolName, input, resp, err, false, false)
	return resp, nil
}

func validateRuntimeUpdatePlan(req *runtimeUpdatePlanRequest) error {
	if req == nil || len(req.Plan) == 0 {
		return fmt.Errorf("plan is required")
	}
	if len(req.Plan) > 20 {
		return fmt.Errorf("plan cannot exceed 20 steps")
	}
	req.Explanation = strings.TrimSpace(req.Explanation)
	for index := range req.Plan {
		step := &req.Plan[index]
		step.Step = strings.TrimSpace(step.Step)
		step.Status = strings.TrimSpace(step.Status)
		switch step.Status {
		case "inProgress", "in-progress":
			step.Status = "in_progress"
		}
		if step.Step == "" {
			return fmt.Errorf("plan step %d step is required", index+1)
		}
		switch step.Status {
		case "pending", "in_progress", "completed":
		default:
			return fmt.Errorf("plan step %d status must be pending, in_progress, or completed", index+1)
		}
	}
	return nil
}

func runtimeInteractionFromTool(run *agentcore.AgentRun, toolName string, input json.RawMessage) (*agentcore.AgentRunInteraction, string, string, error) {
	if run == nil {
		return nil, "", "", fmt.Errorf("agent run is required")
	}
	interaction := &agentcore.AgentRunInteraction{
		AppID:       run.AppID,
		RunID:       run.ID,
		RuntimeKind: run.RuntimeKind,
		Status:      "pending",
	}
	switch tools.CanonicalName(toolName) {
	case runtimeToolRequestUserInput:
		var req runtimeUserInputRequest
		if err := json.Unmarshal(input, &req); err != nil {
			return nil, "", "", fmt.Errorf("parse request_user_input input: %w", err)
		}
		if len(req.Questions) == 0 {
			return nil, "", "", fmt.Errorf("request_user_input requires at least one question")
		}
		interaction.InteractionKind = "request_user_input"
		interaction.Title = "Input requested"
		interaction.Summary = "The agent needs input before it can continue."
		interaction.RequestPayload = runtimeInteractionRequestPayload("request_user_input_v1", input)
		return interaction, "human_input_request", agentcore.PauseReasonHumanInput, nil
	case runtimeToolRequestApproval, runtimeToolRequestReviewCheckpoint:
		var req runtimeApprovalRequest
		if err := json.Unmarshal(input, &req); err != nil {
			return nil, "", "", fmt.Errorf("parse %s input: %w", toolName, err)
		}
		if strings.TrimSpace(req.Title) == "" {
			return nil, "", "", fmt.Errorf("%s requires title", toolName)
		}
		interaction.InteractionKind = "approval_request"
		if tools.CanonicalName(toolName) == runtimeToolRequestReviewCheckpoint {
			interaction.InteractionKind = "review_checkpoint"
		}
		interaction.Title = strings.TrimSpace(req.Title)
		interaction.Summary = strings.TrimSpace(req.Summary)
		interaction.RequestPayload = runtimeInteractionRequestPayload(interactionRequestSchema(interaction.InteractionKind), input)
		return interaction, "human_approval_request", agentcore.PauseReasonHumanApproval, nil
	default:
		return nil, "", "", fmt.Errorf("unsupported interaction tool %q", toolName)
	}
}

func runtimeInteractionRequestPayload(schema string, input json.RawMessage) json.RawMessage {
	var body map[string]any
	if err := json.Unmarshal(input, &body); err != nil || body == nil {
		body = map[string]any{}
	}
	body["schema"] = "agent_runtime.v1"
	body["request_schema"] = schema
	body["raw_input"] = json.RawMessage(input)
	payload, _ := json.Marshal(body)
	return payload
}

func interactionRequestSchema(kind string) string {
	if kind == "review_checkpoint" {
		return "review_checkpoint_v1"
	}
	return "approval_request_v1"
}
