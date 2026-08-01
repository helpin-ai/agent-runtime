package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/id"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

var codexPlainTextQuestionLinePattern = regexp.MustCompile(`^(?:\d+[\.\)]|[-*])\s+(.+\?)$`)

var codexPlainTextQuestionPrefixPattern = regexp.MustCompile(`(?i)^(?:which|what|who|when|where|why|how|should|do|does|did|is|are|would|could|can|will|please\s+(?:choose|confirm|specify|select))\b`)

var codexTrailingPlainTextQuestionPattern = regexp.MustCompile(`(?i)(?:^|[.!]\s+)((?:which|what|who|when|where|why|how|should|do|does|did|is|are|would|could|can|will|please\s+(?:choose|confirm|specify|select))\b[^?]*\?)$`)

func latestPendingRuntimeInteraction(ctx context.Context, execCtx *ExecutionContext) (*codexPendingInteraction, bool, bool, error) {
	if execCtx == nil || execCtx.Store == nil || execCtx.Run == nil {
		return nil, false, false, nil
	}
	interactions, err := execCtx.Store.ListInteractions(ctx, execCtx.Run.AppID, execCtx.Run.ID)
	if err != nil {
		return nil, false, false, fmt.Errorf("list runtime interactions: %w", err)
	}
	for idx := len(interactions) - 1; idx >= 0; idx-- {
		interaction := interactions[idx]
		if strings.TrimSpace(interaction.Status) != "pending" {
			continue
		}
		// App-server approval/input interactions are paused immediately when the
		// JSON-RPC request arrives. They may still be pending in adapter-level
		// tests or eventually-consistent stores after the response is replayed,
		// so only MCP-created canonical interaction kinds are handled here.
		rawKind := strings.TrimSpace(interaction.InteractionKind)
		if rawKind == "human_input" || rawKind == "human_approval" {
			continue
		}
		kind := normalizeCodexRuntimeInteractionKind(interaction.InteractionKind)
		switch kind {
		case skills.InteractionKindRequestUserInput:
			return &codexPendingInteraction{ID: interaction.ID, Kind: kind}, false, true, nil
		case skills.InteractionKindApprovalRequest, skills.InteractionKindReviewCheckpoint:
			return &codexPendingInteraction{ID: interaction.ID, Kind: kind}, true, false, nil
		case "command_execution_approval", "file_change_approval", "permissions_approval":
			return &codexPendingInteraction{ID: interaction.ID, Kind: kind}, true, false, nil
		}
	}
	return nil, false, false, nil
}

func normalizeCodexRuntimeInteractionKind(kind string) string {
	switch strings.TrimSpace(kind) {
	case "human_input", skills.InteractionKindRequestUserInput:
		return skills.InteractionKindRequestUserInput
	case "human_approval", skills.InteractionKindApprovalRequest:
		return skills.InteractionKindApprovalRequest
	default:
		return strings.TrimSpace(kind)
	}
}

func synthesizeCodexPlainTextUserInput(ctx context.Context, execCtx *ExecutionContext, assistantMessage string) (*codexPendingInteraction, bool, error) {
	if execCtx == nil || execCtx.Run == nil || strings.TrimSpace(execCtx.Run.InvocationMode) != agentcore.InvocationInteractive {
		return nil, false, nil
	}
	if codexCompletionAllowedAfterApproval(execCtx) || !skills.RequestUserInputUsesRuntimeBridge(execCtx.SkillPolicy, agentcore.RuntimeCodex) {
		return nil, false, nil
	}
	questions := extractCodexPlainTextQuestions(assistantMessage)
	if len(questions) == 0 {
		return nil, false, nil
	}

	rawQuestions := make([]map[string]any, 0, len(questions))
	for index, question := range questions {
		rawQuestions = append(rawQuestions, map[string]any{
			"id":       fmt.Sprintf("followup_question_%d", index+1),
			"header":   "Input required",
			"question": question,
			"isOther":  true,
		})
	}
	rawInput := map[string]any{"questions": rawQuestions}
	payload := map[string]any{
		"schema":         "agent_runtime.v1",
		"request_schema": "request_user_input_v1",
		"questions":      rawQuestions,
		"raw_input":      rawInput,
	}
	requestPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("marshal synthesized Codex user input: %w", err)
	}
	interactionID := id.New("int")
	interaction := agentcore.AgentRunInteraction{
		ID:              interactionID,
		InteractionKind: skills.InteractionKindRequestUserInput,
		Status:          "pending",
		Title:           "Input requested",
		Summary:         strings.Join(questions, " "),
		RequestPayload:  requestPayload,
	}
	if execCtx.InteractionBroker != nil {
		if err := execCtx.InteractionBroker.RequestInteraction(ctx, interaction); err != nil {
			return nil, false, fmt.Errorf("request synthesized Codex user input: %w", err)
		}
	} else if execCtx.Store != nil {
		interaction.AppID = execCtx.Run.AppID
		interaction.RunID = execCtx.Run.ID
		interaction.RuntimeKind = execCtx.Run.RuntimeKind
		if err := execCtx.Store.AppendInteraction(ctx, &interaction); err != nil {
			return nil, false, fmt.Errorf("append synthesized Codex user input: %w", err)
		}
	} else {
		return nil, false, nil
	}
	if execCtx.ArtifactWriter != nil {
		artifactInput, _ := json.Marshal(rawInput)
		metadata, _ := json.Marshal(map[string]any{
			"internal":       false,
			"interaction_id": interactionID,
			"source":         "codex_plain_text_input_bridge",
		})
		_ = execCtx.ArtifactWriter.WriteArtifact(ctx, agentcore.AgentRunArtifact{
			ArtifactType:  "human_input_request",
			Format:        "json",
			StorageMode:   "inline",
			InlineContent: string(artifactInput),
			Metadata:      metadata,
		})
	}
	return &codexPendingInteraction{ID: interactionID, Kind: skills.InteractionKindRequestUserInput}, true, nil
}

func extractCodexPlainTextQuestions(message string) []string {
	lines := strings.Split(strings.TrimSpace(message), "\n")
	bulleted := make([]string, 0, len(lines))
	plain := make([]string, 0, len(lines))
	seen := map[string]struct{}{}
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if match := codexPlainTextQuestionLinePattern.FindStringSubmatch(line); len(match) == 2 {
			appendCodexQuestion(&bulleted, seen, match[1])
			continue
		}
		line = strings.TrimSpace(strings.TrimLeft(line, "#>"))
		if match := codexTrailingPlainTextQuestionPattern.FindStringSubmatch(line); len(match) == 2 {
			line = strings.TrimSpace(match[1])
		}
		if !strings.HasSuffix(line, "?") || !codexPlainTextQuestionPrefixPattern.MatchString(line) || isGenericCodexClosingQuestion(line) {
			continue
		}
		appendCodexQuestion(&plain, seen, line)
	}
	if len(bulleted) >= 2 {
		return bulleted
	}
	return append(bulleted, plain...)
}

func appendCodexQuestion(questions *[]string, seen map[string]struct{}, question string) {
	question = strings.TrimSpace(question)
	if question == "" || isGenericCodexClosingQuestion(question) {
		return
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(question), " "))
	if _, ok := seen[normalized]; ok {
		return
	}
	seen[normalized] = struct{}{}
	*questions = append(*questions, question)
}

func isGenericCodexClosingQuestion(question string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(question)), " "))
	switch normalized {
	case "anything else?", "is there anything else?", "would you like anything else?", "does that help?", "sound good?", "okay?", "ok?":
		return true
	default:
		return false
	}
}

func codexCompletionRequiresInteraction(execCtx *ExecutionContext) bool {
	return len(codexCompletionInteractionKinds(execCtx)) > 0
}

func codexCompletionInteractionKinds(execCtx *ExecutionContext) []string {
	if execCtx == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, kind := range skills.CompletionRequiredInteractionKinds(execCtx.SkillPolicy, execCtx.SkillDefinitions) {
		kind = normalizeCodexRuntimeInteractionKind(kind)
		if kind == "" {
			continue
		}
		seen[kind] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for kind := range seen {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func codexCompletionAllowedAfterApproval(execCtx *ExecutionContext) bool {
	intent, _, _ := lastResumePayload(execCtx)
	return strings.TrimSpace(intent) == "approve"
}

func codexCompletionInteractionRetryPrompt(execCtx *ExecutionContext) string {
	kinds := codexCompletionInteractionKinds(execCtx)
	return fmt.Sprintf(
		"System correction: the previous turn completed without the interaction required by the active skills. Continue from the work already completed; do not restart the task. Before completing, create one of these required interactions: %s. If an approval-ready preview was already published, republish the current full preview in this turn, then call request_approval with the matching phase and preview_panel_key as the final action.",
		strings.Join(kinds, ", "),
	)
}

func synthesizeCodexCompletionApproval(ctx context.Context, execCtx *ExecutionContext, assistantMessage string) (*codexPendingInteraction, bool, error) {
	if execCtx == nil || execCtx.Run == nil || execCtx.InteractionBroker == nil {
		return nil, false, nil
	}
	interactionKind := ""
	for _, kind := range codexCompletionInteractionKinds(execCtx) {
		switch kind {
		case skills.InteractionKindApprovalRequest:
			interactionKind = skills.InteractionKindApprovalRequest
		case skills.InteractionKindReviewCheckpoint:
			if interactionKind == "" {
				interactionKind = skills.InteractionKindReviewCheckpoint
			}
		}
	}
	if interactionKind == "" {
		return nil, false, nil
	}

	phase, previewPanelKey, title := CompletionApprovalContextForSkills(execCtx.SkillDefinitions, interactionKind)
	summary := codexCompletionApprovalSummary(assistantMessage)
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
		return nil, false, fmt.Errorf("marshal Codex completion approval: %w", err)
	}
	interactionID := id.New("int")
	if err := execCtx.InteractionBroker.RequestInteraction(ctx, agentcore.AgentRunInteraction{
		ID:              interactionID,
		InteractionKind: interactionKind,
		Status:          "pending",
		Title:           title,
		Summary:         summary,
		RequestPayload:  requestPayload,
	}); err != nil {
		return nil, false, fmt.Errorf("request Codex completion approval: %w", err)
	}
	if execCtx.ArtifactWriter != nil {
		artifactInput, _ := json.Marshal(rawInput)
		metadata, _ := json.Marshal(map[string]any{
			"internal":       false,
			"interaction_id": interactionID,
			"source":         "codex_completion_guard",
		})
		_ = execCtx.ArtifactWriter.WriteArtifact(ctx, agentcore.AgentRunArtifact{
			ArtifactType:  "human_approval_request",
			Format:        "json",
			StorageMode:   "inline",
			InlineContent: string(artifactInput),
			Metadata:      metadata,
		})
	}
	return &codexPendingInteraction{ID: interactionID, Kind: interactionKind}, true, nil
}

// CompletionApprovalContextForSkills binds synthesized approvals only when the
// active skill contract identifies one unambiguous, known preview. This keeps
// Scribe/Planner approvals connected to their preview without imposing
// task-planning semantics on unrelated custom agents that also target tasks.
func CompletionApprovalContextForSkills(definitions []skills.Definition, interactionKind string) (string, string, string) {
	type previewContract struct {
		toolName string
		phase    string
		panelKey string
		title    string
	}
	contracts := []previewContract{
		{toolName: "publish_task_plan_doc", phase: "task_doc", panelKey: "task_plan_doc", title: "Approve task planning document"},
		{toolName: "publish_prd_draft", phase: "prd", panelKey: "prd_draft", title: "Approve PRD"},
		{toolName: "publish_task_plan", phase: "tasks", panelKey: "task_plan", title: "Approve task plan"},
	}
	matched := make([]previewContract, 0, 1)
	for _, contract := range contracts {
		if skillDefinitionsRequireTool(definitions, contract.toolName) {
			matched = append(matched, contract)
		}
	}
	if len(matched) == 1 {
		contract := matched[0]
		return contract.phase, contract.panelKey, contract.title
	}
	if interactionKind == skills.InteractionKindReviewCheckpoint {
		return "review", "", "Review agent result"
	}
	return "approval", "", "Approve agent result"
}

func skillDefinitionsRequireTool(definitions []skills.Definition, toolName string) bool {
	want := tools.CanonicalName(toolName)
	for _, definition := range definitions {
		for _, requiredTool := range definition.RequiredTools {
			if tools.CanonicalName(requiredTool) == want {
				return true
			}
		}
	}
	return false
}

func codexCompletionApprovalSummary(message string) string {
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

func codexInteractionResumePrompt(execCtx *ExecutionContext, pending *codexPendingInteraction) string {
	intent, content, responsePayload := lastResumePayload(execCtx)
	parts := []string{
		fmt.Sprintf("Continue after the human response to the pending %s interaction.", strings.TrimSpace(pending.Kind)),
	}
	switch strings.TrimSpace(intent) {
	case "approve":
		parts = append(parts, "The human approved the pending request. Continue from that approval without asking them to approve the same artifact again. Execute every post-approval action required by the active skills and tool contracts before completing; do not stop with a prose-only acknowledgement of the approval.")
	case "request_changes":
		parts = append(parts, "The human requested changes. Revise the current artifact, republish the full replacement preview, and request approval again when it is ready.")
	case "reply":
		parts = append(parts, "Use the human's reply to continue the current workflow.")
	default:
		if strings.TrimSpace(intent) != "" {
			parts = append(parts, "Resume intent: "+strings.TrimSpace(intent)+".")
		}
	}
	if strings.TrimSpace(content) != "" {
		parts = append(parts, "Human response:\n"+strings.TrimSpace(content))
	}
	if len(responsePayload) > 0 && strings.TrimSpace(string(responsePayload)) != "null" {
		var compact any
		if json.Unmarshal(responsePayload, &compact) == nil {
			if payload, err := json.Marshal(compact); err == nil {
				parts = append(parts, "Structured response: "+string(payload))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
