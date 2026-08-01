package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type codexEventMapper struct {
	execCtx *ExecutionContext
	workDir string

	assistantText      strings.Builder
	assistantMessageID string
	assistantStarted   bool
	assistantCompleted bool

	completedTurn   *codexTurn
	latestDiff      string
	usage           codexTokenUsageBreakdown
	liveTools       map[string]codexLiveToolCall
	toolSummaries   []codexToolSummary
	toolInvocations []nativeToolInvocation
	lastPlan        *codexPlanArtifact

	stdout strings.Builder
	stderr strings.Builder
}

type codexLiveToolCall struct {
	Name    string
	Input   string
	Started time.Time
}

type codexToolSummary struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	Status     string `json:"status,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

type codexPlanArtifact struct {
	Note string          `json:"note,omitempty"`
	Plan []codexPlanStep `json:"plan"`
}

type codexPlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

func newCodexEventMapper(execCtx *ExecutionContext, workDir string) *codexEventMapper {
	return &codexEventMapper{
		execCtx:   execCtx,
		workDir:   strings.TrimSpace(workDir),
		liveTools: map[string]codexLiveToolCall{},
	}
}

func (m *codexEventMapper) HandleNotification(ctx context.Context, method string, params json.RawMessage) error {
	switch strings.TrimSpace(method) {
	case "thread/started":
		var payload codexThreadStartedNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		m.appendStdout(ctx, "Codex session started.\n", true)
	case "turn/started":
		var payload codexTurnStartedNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		m.appendStdout(ctx, "Codex started the turn.\n", true)
	case "thread/tokenUsage/updated":
		var payload codexThreadTokenUsageUpdatedNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		m.usage = payload.TokenUsage.Total
		m.emit(ctx, "usage.checkpoint", map[string]any{
			"usage_semantic": "cumulative",
			"usage": map[string]any{
				"total_tokens":            m.usage.TotalTokens,
				"input_tokens":            m.usage.InputTokens,
				"cached_input_tokens":     m.usage.CachedInputTokens,
				"output_tokens":           m.usage.OutputTokens,
				"reasoning_output_tokens": m.usage.ReasoningOutputTokens,
			},
		})
	case "turn/diff/updated":
		var payload codexTurnDiffUpdatedNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		m.latestDiff = strings.TrimSpace(NormalizeCodexUnifiedDiff(m.workDir, payload.Diff))
	case "turn/plan/updated":
		var payload codexTurnPlanUpdatedNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		plan := codexPlanArtifact{Plan: make([]codexPlanStep, 0, len(payload.Plan))}
		if payload.Explanation != nil {
			plan.Note = strings.TrimSpace(*payload.Explanation)
		}
		for _, step := range payload.Plan {
			if strings.TrimSpace(step.Step) == "" {
				continue
			}
			plan.Plan = append(plan.Plan, codexPlanStep{
				Step:   strings.TrimSpace(step.Step),
				Status: codexPlanStepStatus(step.Status),
			})
		}
		if len(plan.Plan) > 0 {
			m.lastPlan = &plan
			content, _ := json.Marshal(plan)
			m.writeArtifact(ctx, "run_plan", "json", string(content), map[string]any{
				"notify":             true,
				"runtime_kind":       agentcore.RuntimeCodex,
				"codex_request_kind": "turn_plan",
				"codex_turn_id":      strings.TrimSpace(payload.TurnID),
			})
			m.emit(ctx, "plan_updated", map[string]any{
				"content": string(content),
				"plan":    plan.Plan,
				"note":    plan.Note,
			})
		}
	case "item/agentMessage/delta":
		var payload codexAgentMessageDeltaNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		m.appendAssistantDelta(ctx, payload.Delta)
	case "item/commandExecution/outputDelta":
		var payload codexCommandExecutionOutputDeltaNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		m.appendStdout(ctx, payload.Delta, true)
	case "item/started":
		var payload codexItemStartedNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		m.handleItemStarted(ctx, payload.Item)
	case "item/completed":
		var payload codexItemCompletedNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		m.handleItemCompleted(ctx, payload.Item)
	case "error":
		var payload codexErrorNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		if payload.Error != nil && strings.TrimSpace(payload.Error.Message) != "" {
			m.appendStderr(ctx, strings.TrimSpace(payload.Error.Message)+"\n", true)
		}
	case "turn/completed":
		var payload codexTurnCompletedNotification
		if err := json.Unmarshal(params, &payload); err != nil {
			return err
		}
		m.completedTurn = &payload.Turn
		if payload.Turn.Error != nil && strings.TrimSpace(payload.Turn.Error.Message) != "" {
			m.appendStderr(ctx, strings.TrimSpace(payload.Turn.Error.Message)+"\n", true)
		}
		m.completeAssistantStream(ctx)
		m.appendStdout(ctx, "Codex finished the turn.\n", true)
	case "serverRequest/resolved":
		return nil
	default:
		return nil
	}
	return nil
}

func (m *codexEventMapper) FlushArtifacts(ctx context.Context) {
	if m == nil {
		return
	}
	if text := m.stdout.String(); strings.TrimSpace(text) != "" {
		m.writeArtifact(ctx, "codex_stdout", "text", text, map[string]any{
			"notify":       false,
			"runtime_kind": agentcore.RuntimeCodex,
		})
	}
	if text := m.stderr.String(); strings.TrimSpace(text) != "" {
		m.writeArtifact(ctx, "codex_stderr", "text", text, map[string]any{
			"notify":       false,
			"runtime_kind": agentcore.RuntimeCodex,
		})
	}
	if diff := strings.TrimSpace(m.latestDiff); diff != "" {
		m.writeArtifact(ctx, "codex_diff", "patch", diff, map[string]any{
			"notify":       true,
			"runtime_kind": agentcore.RuntimeCodex,
		})
	}
}

func (m *codexEventMapper) AssistantText() string {
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m.assistantText.String())
}

func (m *codexEventMapper) CompletedTurn() *codexTurn {
	if m == nil {
		return nil
	}
	return m.completedTurn
}

func (m *codexEventMapper) OutputSummary() json.RawMessage {
	if m == nil {
		summary, _ := json.Marshal(map[string]any{"runtime_kind": agentcore.RuntimeCodex})
		return summary
	}
	body := map[string]any{
		"runtime_kind":            agentcore.RuntimeCodex,
		"latest_diff":             strings.TrimSpace(m.latestDiff),
		"total_tokens":            m.usage.TotalTokens,
		"input_tokens":            m.usage.InputTokens,
		"cached_input_tokens":     m.usage.CachedInputTokens,
		"output_tokens":           m.usage.OutputTokens,
		"reasoning_output_tokens": m.usage.ReasoningOutputTokens,
		"tool_summaries":          m.toolSummaries,
	}
	if m.lastPlan != nil {
		body["run_plan"] = m.lastPlan
	}
	summary, _ := json.Marshal(body)
	return summary
}

func (m *codexEventMapper) ToolInvocations() json.RawMessage {
	if m == nil {
		return nil
	}
	return marshalNativeToolInvocations(m.toolInvocations)
}

func (m *codexEventMapper) appendAssistantDelta(ctx context.Context, text string) {
	if text == "" {
		return
	}
	if !m.assistantStarted {
		m.assistantStarted = true
		m.emit(ctx, "assistant_message_started", map[string]any{
			"message_id": m.ensureAssistantMessageID(),
		})
	}
	m.assistantText.WriteString(text)
	m.emit(ctx, "assistant_message_delta", map[string]any{
		"message_id": m.ensureAssistantMessageID(),
		"text":       text,
		"content":    text,
	})
}

func (m *codexEventMapper) completeAssistantStream(ctx context.Context) {
	if !m.assistantStarted || m.assistantCompleted {
		return
	}
	m.assistantCompleted = true
	text := strings.TrimSpace(m.assistantText.String())
	m.emit(ctx, "assistant_message_completed", map[string]any{
		"message_id": m.ensureAssistantMessageID(),
		"text":       text,
		"content":    text,
	})
}

func (m *codexEventMapper) ensureAssistantMessageID() string {
	if strings.TrimSpace(m.assistantMessageID) == "" {
		m.assistantMessageID = uuid.NewString()
	}
	return strings.TrimSpace(m.assistantMessageID)
}

func (m *codexEventMapper) AssistantMessageID() string {
	return strings.TrimSpace(m.assistantMessageID)
}

func (m *codexEventMapper) handleItemStarted(ctx context.Context, item codexThreadItem) {
	toolName, input := codexToolEventDetails(m.workDir, item)
	if toolName == "" {
		return
	}
	itemID := strings.TrimSpace(item.ID)
	m.liveTools[itemID] = codexLiveToolCall{
		Name:    toolName,
		Input:   input,
		Started: time.Now(),
	}
	parentMessageID := m.ensureAssistantMessageID()
	argsText := strings.TrimSpace(input)
	m.emit(ctx, "tool_call_started", map[string]any{
		"tool_call_id":      itemID,
		"tool_name":         toolName,
		"tool_input":        input,
		"parent_message_id": parentMessageID,
		"args_text":         argsText,
	})
	if argsText != "" {
		m.emit(ctx, "tool_call_args_delta", map[string]any{
			"tool_call_id":      itemID,
			"tool_name":         toolName,
			"parent_message_id": parentMessageID,
			"args_delta":        argsText,
			"args_text":         argsText,
		})
	}
}

func (m *codexEventMapper) handleItemCompleted(ctx context.Context, item codexThreadItem) {
	if strings.TrimSpace(item.Type) == "agentMessage" {
		text := strings.TrimSpace(item.Text)
		if text != "" && strings.TrimSpace(m.assistantText.String()) == "" {
			m.appendAssistantDelta(ctx, text)
		}
		m.completeAssistantStream(ctx)
		return
	}

	toolName, _ := codexToolEventDetails(m.workDir, item)
	if toolName == "" {
		return
	}
	itemID := strings.TrimSpace(item.ID)
	live := m.liveTools[itemID]
	delete(m.liveTools, itemID)
	durationMs := item.DurationMs
	if durationMs == nil && !live.Started.IsZero() {
		derived := time.Since(live.Started).Milliseconds()
		durationMs = &derived
	}
	outputSummary := codexToolOutputSummary(m.workDir, item)
	parentMessageID := m.ensureAssistantMessageID()
	resultMessageID := uuid.NewString()
	errorText := ""
	if codexItemFailed(item) {
		errorText = outputSummary
	}
	m.emit(ctx, "tool_call_result", map[string]any{
		"tool_call_id":      itemID,
		"tool_name":         toolName,
		"parent_message_id": parentMessageID,
		"result_message_id": resultMessageID,
		"content":           outputSummary,
		"output_summary":    outputSummary,
		"error":             errorText,
	})
	m.emit(ctx, "tool_call_finished", map[string]any{
		"tool_call_id":      itemID,
		"tool_name":         toolName,
		"parent_message_id": parentMessageID,
		"result_message_id": resultMessageID,
		"output_summary":    outputSummary,
		"content":           outputSummary,
		"duration_ms":       derefInt64(durationMs),
		"error":             errorText,
	})
	if strings.TrimSpace(item.Type) == "fileChange" && m.latestDiff == "" {
		m.latestDiff = codexDiffFromFileChange(m.workDir, item)
	}
	m.toolSummaries = append(m.toolSummaries, codexToolSummary{
		ID:         itemID,
		Name:       toolName,
		Status:     strings.TrimSpace(item.Status),
		Summary:    strings.TrimSpace(outputSummary),
		Error:      errorText,
		DurationMs: derefInt64(durationMs),
	})
	input, _ := json.Marshal(codexToolCallInput(item, toolName))
	m.toolInvocations = append(m.toolInvocations, nativeToolInvocation{
		ToolName:      toolName,
		Input:         input,
		OutputSummary: strings.TrimSpace(outputSummary),
		DurationMs:    derefInt64(durationMs),
	})
	m.recordToolCall(ctx, item, toolName, outputSummary, errorText)
}

func (m *codexEventMapper) appendStdout(ctx context.Context, text string, notify bool) {
	if text == "" {
		return
	}
	m.stdout.WriteString(text)
	m.writeArtifact(ctx, "codex_stdout_chunk", "text", text, map[string]any{
		"notify":       notify,
		"runtime_kind": agentcore.RuntimeCodex,
	})
}

func (m *codexEventMapper) appendStderr(ctx context.Context, text string, notify bool) {
	if text == "" {
		return
	}
	m.stderr.WriteString(text)
	m.writeArtifact(ctx, "codex_stderr_chunk", "text", text, map[string]any{
		"notify":       notify,
		"runtime_kind": agentcore.RuntimeCodex,
	})
}

func (m *codexEventMapper) writeArtifact(ctx context.Context, artifactType string, format string, content string, metadata map[string]any) {
	if m == nil || m.execCtx == nil || m.execCtx.ArtifactWriter == nil {
		return
	}
	meta, _ := json.Marshal(metadata)
	_ = m.execCtx.ArtifactWriter.WriteArtifact(ctx, agentcore.AgentRunArtifact{
		ArtifactType:  strings.TrimSpace(artifactType),
		Format:        strings.TrimSpace(format),
		StorageMode:   "inline",
		InlineContent: content,
		Metadata:      meta,
	})
}

func (m *codexEventMapper) emit(ctx context.Context, eventType string, data map[string]any) {
	if m == nil || m.execCtx == nil || m.execCtx.EventSink == nil || m.execCtx.Run == nil {
		return
	}
	m.execCtx.EventSink.Emit(ctx, Event{
		AppID: m.execCtx.Run.AppID,
		RunID: m.execCtx.Run.ID,
		Type:  eventType,
		Data:  data,
	})
}

func (m *codexEventMapper) recordToolCall(ctx context.Context, item codexThreadItem, toolName string, outputSummary string, errorText string) {
	if m == nil || m.execCtx == nil || m.execCtx.Store == nil || m.execCtx.Run == nil {
		return
	}
	input, _ := json.Marshal(codexToolCallInput(item, toolName))
	output, _ := json.Marshal(codexToolCallOutput(item, outputSummary, errorText))
	_ = m.execCtx.Store.AppendToolCall(ctx, &agentcore.ToolCall{
		AppID:            m.execCtx.Run.AppID,
		RunID:            m.execCtx.Run.ID,
		ToolName:         toolName,
		Input:            input,
		Output:           output,
		Error:            strings.TrimSpace(errorText),
		Mutating:         codexToolCallMutating(item),
		ApprovalRequired: strings.EqualFold(strings.TrimSpace(item.Status), "declined"),
	})
}

func codexToolCallInput(item codexThreadItem, toolName string) map[string]any {
	input := map[string]any{
		"runtime_kind":  agentcore.RuntimeCodex,
		"tool_name":     strings.TrimSpace(toolName),
		"codex_item_id": strings.TrimSpace(item.ID),
		"codex_type":    strings.TrimSpace(item.Type),
	}
	switch strings.TrimSpace(item.Type) {
	case "commandExecution":
		input["command"] = strings.TrimSpace(item.Command)
		input["cwd"] = strings.TrimSpace(item.Cwd)
	case "fileChange":
		input["cwd"] = strings.TrimSpace(item.Cwd)
		input["changes"] = codexFileChangeInputs(item.Changes)
	case "mcpToolCall":
		input["server"] = strings.TrimSpace(item.Server)
		input["tool"] = strings.TrimSpace(item.Tool)
		input["arguments"] = json.RawMessage(item.Arguments)
	case "dynamicToolCall":
		input["tool"] = strings.TrimSpace(item.Tool)
		input["arguments"] = json.RawMessage(item.Arguments)
	}
	return input
}

func codexToolCallOutput(item codexThreadItem, outputSummary string, errorText string) map[string]any {
	output := map[string]any{
		"runtime_kind":  agentcore.RuntimeCodex,
		"codex_item_id": strings.TrimSpace(item.ID),
		"codex_type":    strings.TrimSpace(item.Type),
		"status":        strings.TrimSpace(item.Status),
		"summary":       strings.TrimSpace(outputSummary),
		"error":         strings.TrimSpace(errorText),
	}
	if item.ExitCode != nil {
		output["exit_code"] = *item.ExitCode
	}
	if item.DurationMs != nil {
		output["duration_ms"] = *item.DurationMs
	}
	if item.AggregatedOutput != nil {
		output["aggregated_output"] = *item.AggregatedOutput
	}
	if item.Success != nil {
		output["success"] = *item.Success
	}
	if len(item.Result) > 0 {
		output["result"] = json.RawMessage(item.Result)
	}
	if len(item.Changes) > 0 {
		output["changes"] = item.Changes
	}
	if item.Error != nil && strings.TrimSpace(item.Error.Message) != "" {
		output["tool_error"] = strings.TrimSpace(item.Error.Message)
	}
	return output
}

func codexFileChangeInputs(changes []codexFileChange) []map[string]any {
	out := make([]map[string]any, 0, len(changes))
	for _, change := range changes {
		out = append(out, map[string]any{
			"path": strings.TrimSpace(change.Path),
			"kind": json.RawMessage(change.Kind),
		})
	}
	return out
}

func codexToolCallMutating(item codexThreadItem) bool {
	switch strings.TrimSpace(item.Type) {
	case "commandExecution", "fileChange":
		return true
	default:
		return false
	}
}

func writeCodexConfigArtifact(ctx context.Context, execCtx *ExecutionContext, workDir string, cfg CodexConfig, state *codexSessionState) {
	if execCtx == nil || execCtx.ArtifactWriter == nil {
		return
	}
	body := map[string]any{
		"runtime_kind":       agentcore.RuntimeCodex,
		"work_dir":           strings.TrimSpace(workDir),
		"model_provider":     firstNonEmpty(cfg.ModelProvider, agentProvider(execCtx), "openai"),
		"model":              firstNonEmpty(cfg.Model, agentModel(execCtx)),
		"sandbox":            firstNonEmpty(cfg.Sandbox, "workspace-write"),
		"approval_policy":    firstNonEmpty(cfg.ApprovalPolicy, "on-request"),
		"approvals_reviewer": firstNonEmpty(cfg.ApprovalsReviewer, "user"),
		"openai_auth_mode":   strings.TrimSpace(cfg.OpenAIAuthMode),
	}
	if state != nil {
		body["thread_id"] = strings.TrimSpace(state.ThreadID)
		body["provider"] = strings.TrimSpace(state.Provider)
		body["auth_mode"] = strings.TrimSpace(state.AuthMode)
		body["codex_home"] = strings.TrimSpace(state.CodexHome)
	}
	content, _ := json.Marshal(body)
	meta, _ := json.Marshal(map[string]any{
		"notify":       false,
		"runtime_kind": agentcore.RuntimeCodex,
	})
	_ = execCtx.ArtifactWriter.WriteArtifact(ctx, agentcore.AgentRunArtifact{
		ArtifactType:  "codex_config",
		Format:        "json",
		StorageMode:   "inline",
		InlineContent: string(content),
		Metadata:      meta,
	})
}

func agentProvider(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.Agent == nil {
		return ""
	}
	return execCtx.Agent.Provider
}

func agentModel(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.Agent == nil {
		return ""
	}
	return execCtx.Agent.Model
}

func codexPlanStepStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "completed":
		return "completed"
	case "inprogress", "in_progress":
		return "in_progress"
	default:
		return "pending"
	}
}

func codexToolEventDetails(workDir string, item codexThreadItem) (string, string) {
	switch strings.TrimSpace(item.Type) {
	case "commandExecution":
		command := strings.TrimSpace(item.Command)
		if command == "" {
			command = "command"
		}
		return "run_command", command
	case "fileChange":
		return "apply_patch", codexDiffFromFileChange(workDir, item)
	case "mcpToolCall":
		name := strings.TrimSpace(item.Tool)
		if server := strings.TrimSpace(item.Server); server != "" && name != "" {
			name = server + "/" + name
		}
		return firstNonEmpty(name, "mcp_tool_call"), strings.TrimSpace(string(item.Arguments))
	case "dynamicToolCall":
		return firstNonEmpty(strings.TrimSpace(item.Tool), "dynamic_tool_call"), strings.TrimSpace(string(item.Arguments))
	default:
		return "", ""
	}
}

func codexToolOutputSummary(workDir string, item codexThreadItem) string {
	switch strings.TrimSpace(item.Type) {
	case "commandExecution":
		if item.AggregatedOutput != nil && strings.TrimSpace(*item.AggregatedOutput) != "" {
			return truncateCodexText(strings.TrimSpace(*item.AggregatedOutput), 4000)
		}
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case "completed":
			if item.ExitCode != nil {
				return fmt.Sprintf("command completed with exit code %d", *item.ExitCode)
			}
			return "command completed"
		case "declined":
			return "command approval was declined"
		case "failed":
			if item.ExitCode != nil {
				return fmt.Sprintf("command failed with exit code %d", *item.ExitCode)
			}
			return "command failed"
		default:
			return strings.TrimSpace(item.Status)
		}
	case "fileChange":
		diff := codexDiffFromFileChange(workDir, item)
		if diff != "" {
			return truncateCodexText(diff, 4000)
		}
		if count := len(item.Changes); count > 0 {
			return fmt.Sprintf("%d file change(s)", count)
		}
		return firstNonEmpty(strings.TrimSpace(item.Status), "file changes completed")
	case "mcpToolCall":
		if item.Error != nil && strings.TrimSpace(item.Error.Message) != "" {
			return strings.TrimSpace(item.Error.Message)
		}
		if strings.TrimSpace(string(item.Result)) != "" {
			return truncateCodexText(strings.TrimSpace(string(item.Result)), 4000)
		}
		return firstNonEmpty(strings.TrimSpace(item.Status), "mcp tool call completed")
	case "dynamicToolCall":
		if item.Error != nil && strings.TrimSpace(item.Error.Message) != "" {
			return strings.TrimSpace(item.Error.Message)
		}
		if strings.TrimSpace(string(item.Result)) != "" {
			return truncateCodexText(strings.TrimSpace(string(item.Result)), 4000)
		}
		if item.Success != nil {
			return fmt.Sprintf("dynamic tool success=%t", *item.Success)
		}
		return firstNonEmpty(strings.TrimSpace(item.Status), "dynamic tool call completed")
	default:
		return ""
	}
}

func codexDiffFromFileChange(workDir string, item codexThreadItem) string {
	if len(item.Changes) == 0 {
		return ""
	}
	converted := CodexThreadItem{Type: item.Type, Cwd: item.Cwd}
	for _, change := range item.Changes {
		converted.Changes = append(converted.Changes, CodexFileChange{
			Path: change.Path,
			Diff: change.Diff,
		})
	}
	return CodexDiffFromFileChange(workDir, converted)
}

func codexItemFailed(item codexThreadItem) bool {
	switch strings.ToLower(strings.TrimSpace(item.Status)) {
	case "failed", "declined":
		return true
	default:
		return item.Error != nil && strings.TrimSpace(item.Error.Message) != ""
	}
}

func truncateCodexText(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
