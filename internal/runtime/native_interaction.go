package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const (
	nativeToolUpdatePlan              = "update_plan"
	nativeToolRequestUserInput        = "request_user_input"
	nativeToolRequestApproval         = "request_approval"
	nativeToolRequestReviewCheckpoint = "request_review_checkpoint"

	nativeInteractionKindRequestUserInput = "request_user_input"
	nativeInteractionKindApprovalRequest  = "approval_request"
	nativeInteractionKindReviewCheckpoint = "review_checkpoint"

	nativeInteractionSchemaAgentRuntimeV1   = "agent_runtime.v1"
	nativeInteractionSchemaRequestInputV1   = "request_user_input_v1"
	nativeInteractionSchemaApprovalV1       = "approval_request_v1"
	nativeInteractionSchemaReviewCheckpoint = "review_checkpoint_v1"

	nativeQuestionTypeSingleSelect = "single_select"
)

var nativeInteractionOptionSlugSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

type nativeUserInputOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type nativeUserInputQuestion struct {
	ID       string                  `json:"id"`
	Header   string                  `json:"header,omitempty"`
	Question string                  `json:"question"`
	IsOther  bool                    `json:"isOther,omitempty"`
	IsSecret bool                    `json:"isSecret,omitempty"`
	Options  []nativeUserInputOption `json:"options,omitempty"`
}

type nativeUserInputRequest struct {
	Questions []nativeUserInputQuestion `json:"questions"`
}

type nativeHumanInputOption struct {
	Value    string `json:"value"`
	Label    string `json:"label"`
	Freetext bool   `json:"freetext,omitempty"`
}

type nativeHumanInputQuestion struct {
	ID      string                   `json:"id"`
	Type    string                   `json:"type,omitempty"`
	Text    string                   `json:"text"`
	Options []nativeHumanInputOption `json:"options"`
}

type nativeHumanInputRequest struct {
	Questions []nativeHumanInputQuestion `json:"questions"`
}

type nativeApprovalRequest struct {
	Phase           string `json:"phase,omitempty"`
	PreviewPanelKey string `json:"preview_panel_key,omitempty"`
	Title           string `json:"title"`
	Summary         string `json:"summary,omitempty"`
	// Action carries the exact structured content being approved (for
	// example a host-side launch payload). Hosts can verify the executed
	// action matches the approved one.
	Action json.RawMessage `json:"action,omitempty"`
}

type nativeReviewCheckpointFinding struct {
	ID           string  `json:"id,omitempty"`
	Title        string  `json:"title"`
	Body         string  `json:"body"`
	Priority     string  `json:"priority,omitempty"`
	Confidence   float64 `json:"confidence,omitempty"`
	CodeLocation string  `json:"code_location,omitempty"`
}

type nativeReviewCheckpointRequest struct {
	Phase                  string                          `json:"phase,omitempty"`
	PreviewPanelKey        string                          `json:"preview_panel_key,omitempty"`
	Title                  string                          `json:"title"`
	Summary                string                          `json:"summary,omitempty"`
	Findings               []nativeReviewCheckpointFinding `json:"findings,omitempty"`
	OverallCorrectness     string                          `json:"overall_correctness,omitempty"`
	OverallExplanation     string                          `json:"overall_explanation,omitempty"`
	OverallConfidenceScore float64                         `json:"overall_confidence_score,omitempty"`
}

type nativePlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

type nativeUpdatePlanRequest struct {
	Explanation string           `json:"explanation,omitempty"`
	Plan        []nativePlanStep `json:"plan"`
	Metadata    map[string]any   `json:"metadata,omitempty"`
}

func nativeAllowedInteractionToolDefinitions(execCtx *ExecutionContext, existing []tools.Definition) []tools.Definition {
	if execCtx == nil || len(execCtx.AllowedTools) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(existing))
	for _, def := range existing {
		seen[tools.CanonicalName(def.Name)] = true
	}
	definitions := nativeInteractionToolDefinitions()
	out := make([]tools.Definition, 0, len(definitions))
	for _, def := range definitions {
		name := tools.CanonicalName(def.Name)
		if execCtx.AllowedTools[name] && !seen[name] {
			out = append(out, def)
		}
	}
	return out
}

func nativeInteractionToolDefinitions() []tools.Definition {
	return []tools.Definition{
		{
			Name:        nativeToolUpdatePlan,
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
						"description":          "Optional generic planning metadata such as intent, data_sources, expected_outputs, or constraints.",
						"additionalProperties": true,
					},
				},
				"required":             []string{"plan"},
				"additionalProperties": false,
			},
		},
		{
			Name:        nativeToolRequestUserInput,
			Description: "Present structured questions to the human and pause the run until they answer.",
			Category:    "Interaction",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"questions": map[string]any{
						"type":        "array",
						"description": "Questions to present. Prefer 1-3 focused questions per request.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":       map[string]any{"type": "string", "description": "Stable question identifier."},
								"header":   map[string]any{"type": "string", "description": "Optional short heading shown above the question."},
								"question": map[string]any{"type": "string", "description": "The prompt to answer."},
								"isOther":  map[string]any{"type": "boolean", "description": "When true, allow a freeform Other answer."},
								"isSecret": map[string]any{"type": "boolean", "description": "When true, render the answer field as secret input."},
								"options": map[string]any{
									"type":        "array",
									"description": "Optional mutually exclusive answer choices.",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"label":       map[string]any{"type": "string", "description": "User-facing option label."},
											"description": map[string]any{"type": "string", "description": "Optional short description for the option."},
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
			Mutating: true,
		},
		{
			Name:        nativeToolRequestApproval,
			Description: "Request inline approval or change feedback and pause the run until the human responds.",
			Category:    "Interaction",
			InputSchema: nativeApprovalToolSchema(),
			Mutating:    true,
		},
		{
			Name:        nativeToolRequestReviewCheckpoint,
			Description: "Request an inline review checkpoint and pause the run until approval or change feedback.",
			Category:    "Interaction",
			InputSchema: nativeReviewCheckpointToolSchema(),
			Mutating:    true,
		},
	}
}

func nativeApprovalToolSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"phase":             map[string]any{"type": "string", "description": "Short workflow phase label such as prd, tasks, task_doc, or dock_plan_confirm."},
			"preview_panel_key": map[string]any{"type": "string", "description": "Optional preview panel key this approval request refers to."},
			"title":             map[string]any{"type": "string", "description": "User-facing title for the approval request."},
			"summary":           map[string]any{"type": "string", "description": "Optional short approval summary."},
			"action": map[string]any{
				"type":                 "object",
				"description":          "Optional structured content being approved (for example the exact parameters of a follow-up tool call). Hosts verify the executed action matches this object, so include it exactly as you will pass it.",
				"additionalProperties": true,
			},
		},
		"required":             []string{"title"},
		"additionalProperties": false,
	}
}

// nativeApprovalPayloadBody builds the approval request payload body,
// including the structured action when the caller supplied one.
func nativeApprovalPayloadBody(req nativeApprovalRequest) map[string]any {
	body := map[string]any{
		"phase":             req.Phase,
		"preview_panel_key": req.PreviewPanelKey,
		"title":             req.Title,
		"summary":           req.Summary,
	}
	if len(req.Action) > 0 {
		body["action"] = json.RawMessage(req.Action)
	}
	return body
}

func nativeReviewCheckpointToolSchema() map[string]any {
	schema := nativeApprovalToolSchema()
	properties, _ := schema["properties"].(map[string]any)
	properties["findings"] = map[string]any{
		"type":        "array",
		"description": "Optional structured review findings to persist alongside the checkpoint.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":            map[string]any{"type": "string"},
				"title":         map[string]any{"type": "string"},
				"body":          map[string]any{"type": "string"},
				"priority":      map[string]any{"type": "string"},
				"confidence":    map[string]any{"type": "number"},
				"code_location": map[string]any{"type": "string"},
			},
			"required":             []string{"title", "body"},
			"additionalProperties": false,
		},
	}
	properties["overall_correctness"] = map[string]any{"type": "string"}
	properties["overall_explanation"] = map[string]any{"type": "string"}
	properties["overall_confidence_score"] = map[string]any{"type": "number"}
	return schema
}

func nativeIsInteractionTool(name string) bool {
	switch tools.CanonicalName(name) {
	case nativeToolUpdatePlan, nativeToolRequestUserInput, nativeToolRequestApproval, nativeToolRequestReviewCheckpoint:
		return true
	default:
		return false
	}
}

func executeNativeInteractionTool(ctx context.Context, execCtx *ExecutionContext, toolCall NativeBlock) nativeExecutedToolCall {
	name := tools.CanonicalName(toolCall.ToolName)
	input := normalizeNativeToolInput(toolCall.Input)
	executed := nativeExecutedToolCall{
		ToolCallID: strings.TrimSpace(toolCall.ToolCallID),
		ToolName:   name,
		Input:      input,
		Mutating:   name != nativeToolUpdatePlan,
	}
	output, pauseReason, interactionID, err := nativeInteractionToolOutput(ctx, execCtx, name, input)
	executed.Output = strings.TrimSpace(output)
	executed.PauseReason = pauseReason
	executed.InteractionID = interactionID
	if err != nil {
		executed.IsError = true
		executed.Output = err.Error()
		executed.PauseReason = ""
		executed.InteractionID = ""
	}
	return executed
}

func nativeRequiresApproval(execCtx *ExecutionContext) bool {
	if execCtx == nil || execCtx.Agent == nil {
		return true
	}
	switch strings.TrimSpace(execCtx.Agent.ApprovalMode) {
	case "", agentcore.ApprovalModeNever:
		return false
	default:
		return true
	}
}

func nativeRequestToolApproval(ctx context.Context, execCtx *ExecutionContext, toolName string, input json.RawMessage) (string, string, error) {
	toolName = tools.CanonicalName(toolName)
	interactionID := uuid.NewString()
	payload := nativeInteractionRequestPayload(nativeInteractionSchemaApprovalV1, map[string]any{
		"tool_name": toolName,
		"mutating":  true,
		"title":     "Approve tool call",
		"summary":   fmt.Sprintf("Approve %s for this agent run.", toolName),
		"input":     json.RawMessage(input),
	}, input)
	if err := nativePersistInteraction(ctx, execCtx, agentcore.AgentRunInteraction{
		ID:              interactionID,
		InteractionKind: nativeInteractionKindApprovalRequest,
		Status:          "pending",
		Title:           "Approve tool call",
		Summary:         fmt.Sprintf("Approve %s for this agent run.", toolName),
		RequestPayload:  payload,
	}); err != nil {
		return "", "", err
	}
	nativeWriteInteractionArtifact(ctx, execCtx, "human_approval_request", interactionID, json.RawMessage(input))
	return nativeCompactJSON(map[string]any{
		"status":             agentcore.RunStatusPaused,
		"pause_reason":       agentcore.PauseReasonHumanApproval,
		"interaction_id":     interactionID,
		"interaction_kind":   nativeInteractionKindApprovalRequest,
		"approval_required":  true,
		"tool_name":          toolName,
		"approval_request":   "tool_call",
		"approval_summary":   fmt.Sprintf("Approve %s for this agent run.", toolName),
		"approval_title":     "Approve tool call",
		"tool_input_preview": json.RawMessage(input),
	}), interactionID, nil
}

func nativeInteractionToolOutput(ctx context.Context, execCtx *ExecutionContext, name string, input json.RawMessage) (string, string, string, error) {
	switch tools.CanonicalName(name) {
	case nativeToolUpdatePlan:
		var req nativeUpdatePlanRequest
		if err := json.Unmarshal(input, &req); err != nil {
			return "", "", "", fmt.Errorf("parse input: %w", err)
		}
		if err := validateNativeUpdatePlanRequest(&req); err != nil {
			return "", "", "", err
		}
		payload, err := nativeUpdatePlan(ctx, execCtx, req)
		return payload, "", "", err
	case nativeToolRequestUserInput:
		var req nativeUserInputRequest
		if err := json.Unmarshal(input, &req); err != nil {
			return "", "", "", fmt.Errorf("parse input: %w", err)
		}
		if err := validateNativeUserInputRequest(&req); err != nil {
			var legacy nativeHumanInputRequest
			if legacyErr := json.Unmarshal(input, &legacy); legacyErr != nil {
				return "", "", "", err
			}
			if legacyErr := validateNativeLegacyHumanInputRequest(&legacy); legacyErr != nil {
				return "", "", "", err
			}
			req = convertNativeLegacyHumanInputRequest(&legacy)
		}
		payload, err := nativeRequestUserInput(ctx, execCtx, req, input)
		return payload, agentcore.PauseReasonHumanInput, stringFromJSONField(payload, "interaction_id"), err
	case nativeToolRequestApproval:
		var req nativeApprovalRequest
		if err := json.Unmarshal(input, &req); err != nil {
			return "", "", "", fmt.Errorf("parse input: %w", err)
		}
		if err := validateNativeApprovalRequest(&req); err != nil {
			return "", "", "", err
		}
		payload, err := nativeRequestApproval(ctx, execCtx, req, input, nativeInteractionKindApprovalRequest, nativeInteractionSchemaApprovalV1)
		return payload, agentcore.PauseReasonHumanApproval, stringFromJSONField(payload, "interaction_id"), err
	case nativeToolRequestReviewCheckpoint:
		var req nativeReviewCheckpointRequest
		if err := json.Unmarshal(input, &req); err != nil {
			return "", "", "", fmt.Errorf("parse input: %w", err)
		}
		if err := validateNativeReviewCheckpointRequest(&req); err != nil {
			return "", "", "", err
		}
		payload, err := nativeRequestReviewCheckpoint(ctx, execCtx, req, input)
		return payload, agentcore.PauseReasonHumanApproval, stringFromJSONField(payload, "interaction_id"), err
	default:
		return "", "", "", fmt.Errorf("unsupported interaction tool %q", name)
	}
}

func nativeUpdatePlan(ctx context.Context, execCtx *ExecutionContext, req nativeUpdatePlanRequest) (string, error) {
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
		return "", err
	}
	if execCtx != nil && execCtx.ArtifactWriter != nil {
		metadata, _ := json.Marshal(map[string]any{
			"internal": false,
			"source":   "update_plan",
		})
		if err := execCtx.ArtifactWriter.WriteArtifact(ctx, agentcore.AgentRunArtifact{
			ArtifactType:  "run_plan",
			Format:        "json",
			StorageMode:   "inline",
			InlineContent: string(payload),
			Metadata:      metadata,
		}); err != nil {
			return "", err
		}
	}
	emitNativeEvent(ctx, execCtx, "plan_updated", map[string]any{
		"plan":        req.Plan,
		"note":        req.Explanation,
		"explanation": req.Explanation,
		"metadata":    req.Metadata,
	})
	return nativeCompactJSON(map[string]any{
		"status":        "updated",
		"artifact_type": "run_plan",
		"plan":          req.Plan,
		"explanation":   req.Explanation,
		"metadata":      req.Metadata,
	}), nil
}

func nativeRequestUserInput(ctx context.Context, execCtx *ExecutionContext, req nativeUserInputRequest, input json.RawMessage) (string, error) {
	interactionID := uuid.NewString()
	if err := nativePersistInteraction(ctx, execCtx, agentcore.AgentRunInteraction{
		ID:              interactionID,
		InteractionKind: nativeInteractionKindRequestUserInput,
		Status:          "pending",
		Title:           "Input requested",
		Summary:         nativeUserInputSummary(req),
		RequestPayload: nativeInteractionRequestPayload(nativeInteractionSchemaRequestInputV1, map[string]any{
			"questions": req.Questions,
		}, input),
	}); err != nil {
		return "", err
	}
	nativeWriteInteractionArtifact(ctx, execCtx, "human_input_request", interactionID, nativeHumanInputArtifact(req))
	return nativeCompactJSON(map[string]any{
		"status":         agentcore.RunStatusPaused,
		"pause_reason":   agentcore.PauseReasonHumanInput,
		"interaction_id": interactionID,
		"questions":      req.Questions,
	}), nil
}

func nativeRequestApproval(ctx context.Context, execCtx *ExecutionContext, req nativeApprovalRequest, input json.RawMessage, kind string, schema string) (string, error) {
	interactionID := uuid.NewString()
	if err := nativePersistInteraction(ctx, execCtx, agentcore.AgentRunInteraction{
		ID:              interactionID,
		InteractionKind: kind,
		Status:          "pending",
		Title:           req.Title,
		Summary:         req.Summary,
		RequestPayload: nativeInteractionRequestPayload(schema, nativeApprovalPayloadBody(req), input),
	}); err != nil {
		return "", err
	}
	nativeWriteInteractionArtifact(ctx, execCtx, "human_approval_request", interactionID, json.RawMessage(input))
	return nativeCompactJSON(map[string]any{
		"status":            agentcore.RunStatusPaused,
		"pause_reason":      agentcore.PauseReasonHumanApproval,
		"interaction_id":    interactionID,
		"interaction_kind":  kind,
		"phase":             req.Phase,
		"preview_panel_key": req.PreviewPanelKey,
		"title":             req.Title,
		"summary":           req.Summary,
	}), nil
}

func nativeRequestReviewCheckpoint(ctx context.Context, execCtx *ExecutionContext, req nativeReviewCheckpointRequest, input json.RawMessage) (string, error) {
	interactionID := uuid.NewString()
	if err := nativePersistInteraction(ctx, execCtx, agentcore.AgentRunInteraction{
		ID:              interactionID,
		InteractionKind: nativeInteractionKindReviewCheckpoint,
		Status:          "pending",
		Title:           req.Title,
		Summary:         req.Summary,
		RequestPayload: nativeInteractionRequestPayload(nativeInteractionSchemaReviewCheckpoint, map[string]any{
			"phase":                    req.Phase,
			"preview_panel_key":        req.PreviewPanelKey,
			"title":                    req.Title,
			"summary":                  req.Summary,
			"findings":                 req.Findings,
			"overall_correctness":      req.OverallCorrectness,
			"overall_explanation":      req.OverallExplanation,
			"overall_confidence_score": req.OverallConfidenceScore,
		}, input),
	}); err != nil {
		return "", err
	}
	nativeWriteInteractionArtifact(ctx, execCtx, "human_approval_request", interactionID, json.RawMessage(input))
	return nativeCompactJSON(map[string]any{
		"status":                   agentcore.RunStatusPaused,
		"pause_reason":             agentcore.PauseReasonHumanApproval,
		"interaction_id":           interactionID,
		"interaction_kind":         nativeInteractionKindReviewCheckpoint,
		"phase":                    req.Phase,
		"preview_panel_key":        req.PreviewPanelKey,
		"title":                    req.Title,
		"summary":                  req.Summary,
		"findings":                 req.Findings,
		"overall_correctness":      req.OverallCorrectness,
		"overall_explanation":      req.OverallExplanation,
		"overall_confidence_score": req.OverallConfidenceScore,
	}), nil
}

func nativePersistInteraction(ctx context.Context, execCtx *ExecutionContext, interaction agentcore.AgentRunInteraction) error {
	if interaction.Status == "" {
		interaction.Status = "pending"
	}
	if execCtx == nil {
		return fmt.Errorf("execution context is required")
	}
	if execCtx.InteractionBroker != nil {
		return execCtx.InteractionBroker.RequestInteraction(ctx, interaction)
	}
	if execCtx.Store == nil || execCtx.Run == nil {
		return fmt.Errorf("interaction broker is not configured")
	}
	interaction.AppID = execCtx.Run.AppID
	interaction.RunID = execCtx.Run.ID
	interaction.RuntimeKind = execCtx.Run.RuntimeKind
	return execCtx.Store.AppendInteraction(ctx, &interaction)
}

func nativeInteractionRequestPayload(schema string, body map[string]any, rawInput json.RawMessage) json.RawMessage {
	body["schema"] = nativeInteractionSchemaAgentRuntimeV1
	body["request_schema"] = schema
	body["raw_input"] = json.RawMessage(rawInput)
	payload, _ := json.Marshal(body)
	return payload
}

func nativeWriteInteractionArtifact(ctx context.Context, execCtx *ExecutionContext, artifactType string, interactionID string, content any) {
	if execCtx == nil || execCtx.ArtifactWriter == nil {
		return
	}
	payload, err := json.Marshal(content)
	if err != nil {
		return
	}
	metadata, _ := json.Marshal(map[string]any{
		"internal":       false,
		"interaction_id": interactionID,
	})
	_ = execCtx.ArtifactWriter.WriteArtifact(ctx, agentcore.AgentRunArtifact{
		ArtifactType:  artifactType,
		Format:        "json",
		StorageMode:   "inline",
		InlineContent: string(payload),
		Metadata:      metadata,
	})
}

func validateNativeUserInputRequest(req *nativeUserInputRequest) error {
	if req == nil || len(req.Questions) == 0 {
		return fmt.Errorf("questions are required")
	}
	if len(req.Questions) > 10 {
		return fmt.Errorf("questions cannot exceed 10 per request")
	}
	for i := range req.Questions {
		question := &req.Questions[i]
		question.ID = strings.TrimSpace(question.ID)
		question.Header = strings.TrimSpace(question.Header)
		question.Question = strings.TrimSpace(question.Question)
		if question.ID == "" {
			return fmt.Errorf("question id is required")
		}
		if question.Question == "" {
			return fmt.Errorf("question %q question is required", question.ID)
		}
		for j := range question.Options {
			option := &question.Options[j]
			option.Label = strings.TrimSpace(option.Label)
			option.Description = strings.TrimSpace(option.Description)
			if option.Label == "" {
				return fmt.Errorf("question %q option %d label is required", question.ID, j+1)
			}
		}
	}
	return nil
}

func validateNativeUpdatePlanRequest(req *nativeUpdatePlanRequest) error {
	if req == nil || len(req.Plan) == 0 {
		return fmt.Errorf("plan is required")
	}
	if len(req.Plan) > 20 {
		return fmt.Errorf("plan cannot exceed 20 steps")
	}
	req.Explanation = strings.TrimSpace(req.Explanation)
	for i := range req.Plan {
		step := &req.Plan[i]
		step.Step = strings.TrimSpace(step.Step)
		step.Status = strings.TrimSpace(step.Status)
		switch step.Status {
		case "inProgress", "in-progress":
			step.Status = "in_progress"
		}
		if step.Step == "" {
			return fmt.Errorf("plan step %d step is required", i+1)
		}
		switch step.Status {
		case "pending", "in_progress", "completed":
		default:
			return fmt.Errorf("plan step %d status must be pending, in_progress, or completed", i+1)
		}
	}
	return nil
}

func validateNativeLegacyHumanInputRequest(req *nativeHumanInputRequest) error {
	if req == nil || len(req.Questions) == 0 {
		return fmt.Errorf("questions are required")
	}
	if len(req.Questions) > 10 {
		return fmt.Errorf("questions cannot exceed 10 per request")
	}
	for i := range req.Questions {
		question := &req.Questions[i]
		question.ID = strings.TrimSpace(question.ID)
		question.Text = strings.TrimSpace(question.Text)
		question.Type = strings.TrimSpace(question.Type)
		if question.Type == "" {
			question.Type = nativeQuestionTypeSingleSelect
		}
		if question.Type != nativeQuestionTypeSingleSelect {
			return fmt.Errorf("question %q must use type %q", question.ID, nativeQuestionTypeSingleSelect)
		}
		if question.ID == "" {
			return fmt.Errorf("question id is required")
		}
		if question.Text == "" {
			return fmt.Errorf("question %q text is required", question.ID)
		}
		if len(question.Options) == 0 {
			return fmt.Errorf("question %q options are required", question.ID)
		}
		for j := range question.Options {
			option := &question.Options[j]
			option.Value = strings.TrimSpace(option.Value)
			option.Label = strings.TrimSpace(option.Label)
			if option.Value == "" {
				return fmt.Errorf("question %q option %d value is required", question.ID, j+1)
			}
			if option.Label == "" {
				return fmt.Errorf("question %q option %d label is required", question.ID, j+1)
			}
		}
	}
	return nil
}

func validateNativeApprovalRequest(req *nativeApprovalRequest) error {
	if req == nil {
		return fmt.Errorf("title is required")
	}
	normalizeNativeApprovalRequestFields(&req.Phase, &req.PreviewPanelKey, &req.Title, &req.Summary)
	if req.Title == "" {
		return fmt.Errorf("title is required")
	}
	return nil
}

func validateNativeReviewCheckpointRequest(req *nativeReviewCheckpointRequest) error {
	if req == nil {
		return fmt.Errorf("title is required")
	}
	normalizeNativeApprovalRequestFields(&req.Phase, &req.PreviewPanelKey, &req.Title, &req.Summary)
	if req.Title == "" {
		return fmt.Errorf("title is required")
	}
	if req.Phase == "" {
		req.Phase = "review"
	}
	req.OverallCorrectness = strings.TrimSpace(req.OverallCorrectness)
	req.OverallExplanation = strings.TrimSpace(req.OverallExplanation)
	for i := range req.Findings {
		finding := &req.Findings[i]
		finding.ID = strings.TrimSpace(finding.ID)
		if finding.ID == "" {
			finding.ID = fmt.Sprintf("finding_%d", i+1)
		}
		finding.Title = strings.TrimSpace(finding.Title)
		finding.Body = strings.TrimSpace(finding.Body)
		finding.Priority = strings.ToUpper(strings.TrimSpace(finding.Priority))
		finding.CodeLocation = strings.TrimSpace(finding.CodeLocation)
		if finding.Title == "" {
			return fmt.Errorf("finding %d title is required", i+1)
		}
		if finding.Body == "" {
			return fmt.Errorf("finding %d body is required", i+1)
		}
	}
	return nil
}

func normalizeNativeApprovalRequestFields(phase, previewPanelKey, title, summary *string) {
	if phase != nil {
		*phase = strings.ToLower(strings.TrimSpace(*phase))
	}
	if previewPanelKey != nil {
		*previewPanelKey = strings.ToLower(strings.TrimSpace(*previewPanelKey))
	}
	if title != nil {
		*title = strings.TrimSpace(*title)
	}
	if summary != nil {
		*summary = strings.TrimSpace(*summary)
	}
}

func convertNativeLegacyHumanInputRequest(req *nativeHumanInputRequest) nativeUserInputRequest {
	out := nativeUserInputRequest{
		Questions: make([]nativeUserInputQuestion, 0, len(req.Questions)),
	}
	for _, question := range req.Questions {
		item := nativeUserInputQuestion{
			ID:       strings.TrimSpace(question.ID),
			Question: strings.TrimSpace(question.Text),
			Options:  make([]nativeUserInputOption, 0, len(question.Options)),
		}
		for _, option := range question.Options {
			if option.Freetext {
				item.IsOther = true
				continue
			}
			item.Options = append(item.Options, nativeUserInputOption{
				Label: strings.TrimSpace(option.Label),
			})
		}
		out.Questions = append(out.Questions, item)
	}
	return out
}

func nativeUserInputSummary(req nativeUserInputRequest) string {
	if len(req.Questions) == 0 {
		return ""
	}
	question := req.Questions[0]
	header := strings.TrimSpace(question.Header)
	prompt := strings.TrimSpace(question.Question)
	switch {
	case header != "" && prompt != "" && !strings.EqualFold(header, prompt):
		return header + ": " + prompt
	case prompt != "":
		return prompt
	case header != "":
		return header
	case strings.TrimSpace(question.ID) != "":
		return strings.TrimSpace(question.ID)
	default:
		return "Question"
	}
}

func nativeHumanInputArtifact(req nativeUserInputRequest) map[string]any {
	questions := make([]map[string]any, 0, len(req.Questions))
	for _, question := range req.Questions {
		item := map[string]any{
			"id":      strings.TrimSpace(question.ID),
			"type":    nativeQuestionTypeSingleSelect,
			"text":    nativeUserInputQuestionPrompt(question),
			"options": nativeHumanInputOptions(question),
		}
		questions = append(questions, item)
	}
	return map[string]any{"questions": questions}
}

func nativeUserInputQuestionPrompt(question nativeUserInputQuestion) string {
	header := strings.TrimSpace(question.Header)
	prompt := strings.TrimSpace(question.Question)
	switch {
	case header != "" && prompt != "" && !strings.EqualFold(header, prompt):
		return header + ": " + prompt
	case prompt != "":
		return prompt
	default:
		return header
	}
}

func nativeHumanInputOptions(question nativeUserInputQuestion) []map[string]any {
	options := make([]map[string]any, 0, len(question.Options)+1)
	used := map[string]int{}
	for _, option := range question.Options {
		label := strings.TrimSpace(option.Label)
		if label == "" {
			continue
		}
		options = append(options, map[string]any{
			"value": nativeInteractionOptionValue(label, used),
			"label": label,
		})
	}
	if question.IsOther || len(options) == 0 {
		options = append(options, map[string]any{
			"value":    nativeInteractionOptionValue("other", used),
			"label":    "Other",
			"freetext": true,
		})
	}
	return options
}

func nativeInteractionOptionValue(label string, used map[string]int) string {
	base := strings.ToLower(strings.TrimSpace(label))
	base = nativeInteractionOptionSlugSanitizer.ReplaceAllString(base, "_")
	base = strings.Trim(base, "_")
	if base == "" {
		base = "option"
	}
	count := used[base]
	used[base] = count + 1
	if count == 0 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, count+1)
}

func nativeCompactJSON(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(payload)
}

func stringFromJSONField(raw string, field string) string {
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		return ""
	}
	value, _ := body[field].(string)
	return strings.TrimSpace(value)
}
