package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const (
	nativeBlockTypeText       = "text"
	nativeBlockTypeToolCall   = "tool_call"
	nativeBlockTypeToolResult = "tool_result"
)

const (
	defaultNativeMaxToolSteps = 25
	maximumNativeMaxToolSteps = 2000
	nativeToolSummaryLimit    = 500
	nativeToolEventLimit      = 2000
)

type NativeConfig struct {
	ModelFactory NativeModelFactory
	MaxToolSteps int
}

type NativeModelFactory interface {
	ResolveNativeModel(ctx context.Context, execCtx *ExecutionContext, definitions []tools.Definition) (NativeModel, error)
}

type NativeModel interface {
	Generate(ctx context.Context, req NativeModelRequest) (*NativeModelResponse, error)
}

type NativeStreamingModel interface {
	Stream(ctx context.Context, req NativeModelRequest) (NativeModelStream, error)
}

type NativeModelStream interface {
	Recv() (*NativeModelResponse, error)
	Close()
}

type NativeModelRequest struct {
	SystemPrompt string             `json:"system_prompt,omitempty"`
	Messages     []NativeMessage    `json:"messages"`
	Tools        []tools.Definition `json:"tools,omitempty"`
	Step         int                `json:"step"`
}

type NativeModelResponse struct {
	Message      NativeMessage         `json:"message"`
	Usage        NativeUsage           `json:"usage,omitempty"`
	Continuation *ProviderContinuation `json:"continuation,omitempty"`
	Incomplete   bool                  `json:"incomplete,omitempty"`
}

type ProviderContinuation struct {
	Provider           string `json:"provider,omitempty"`
	ResponseID         string `json:"response_id,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	AfterSequenceNo    int    `json:"after_sequence_no,omitempty"`
}

type NativeUsage struct {
	InputTokens           int64 `json:"input_tokens,omitempty"`
	CachedInputTokens     int64 `json:"cached_input_tokens,omitempty"`
	OutputTokens          int64 `json:"output_tokens,omitempty"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens,omitempty"`
}

type NativeMessage struct {
	Role             string        `json:"role"`
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	Blocks           []NativeBlock `json:"blocks,omitempty"`
	ContextSummary   bool          `json:"context_summary,omitempty"`
}

type NativeBlock struct {
	Type                string          `json:"type"`
	Text                string          `json:"text,omitempty"`
	ToolCallID          string          `json:"tool_call_id,omitempty"`
	ToolName            string          `json:"tool_name,omitempty"`
	Input               json.RawMessage `json:"input,omitempty"`
	Output              string          `json:"output,omitempty"`
	IsError             bool            `json:"is_error,omitempty"`
	FinishRejected      bool            `json:"-"`
	FinishAssistantText string          `json:"-"`
}

type nativeExecutionResult struct {
	AssistantText         string
	AssistantMessageID    string
	Messages              []NativeMessage
	Usage                 NativeUsage
	ToolSummaries         []nativeToolSummary
	ToolInvocations       []nativeToolInvocation
	Continuation          *ProviderContinuation
	MaxSteps              bool
	AwaitingInput         bool
	AwaitingApproval      bool
	TurnFinished          bool
	TurnOutcome           string
	CompletionCorrections int
}

type nativeToolSummary struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	Summary    string `json:"summary,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

type nativeToolInvocation struct {
	ToolCallID          string          `json:"tool_call_id"`
	ToolName            string          `json:"tool_name"`
	Input               json.RawMessage `json:"input"`
	OutputSummary       string          `json:"output_summary"`
	DurationMs          int64           `json:"duration_ms"`
	Status              string          `json:"status,omitempty"`
	Error               string          `json:"error,omitempty"`
	AssistantBeforeTool bool            `json:"assistant_before_tool,omitempty"`
}

type nativeExecutedToolCall struct {
	ToolCallID       string
	ToolName         string
	Input            json.RawMessage
	Output           string
	Duration         time.Duration
	IsError          bool
	Mutating         bool
	ApprovalRequired bool
	PauseReason      string
	InteractionID    string
	TurnFinished     bool
	TurnOutcome      string
	FinishSummary    string
}

func executeNativeModel(ctx context.Context, execCtx *ExecutionContext, cfg NativeConfig) (result *nativeExecutionResult, execErr error) {
	if cfg.ModelFactory == nil {
		return nil, fmt.Errorf("native model factory is not configured")
	}
	if execCtx == nil || execCtx.Run == nil || execCtx.Agent == nil {
		return nil, fmt.Errorf("execution context is incomplete")
	}
	maxSteps := nativeMaxToolSteps(execCtx, cfg.MaxToolSteps)
	policy, err := nativeContextPolicy(execCtx)
	if err != nil {
		return nil, err
	}
	recorder, err := openNativeRecorder(ctx, execCtx, policy.Enabled)
	if err != nil {
		return nil, err
	}
	definitions := nativeAllowedToolDefinitions(execCtx)
	model, err := cfg.ModelFactory.ResolveNativeModel(ctx, execCtx, definitions)
	if err != nil {
		return nil, err
	}
	if model == nil {
		return nil, fmt.Errorf("native model factory returned nil model")
	}
	if policy.Enabled {
		if _, ok := model.(NativeSummaryModel); !ok {
			return nil, fmt.Errorf("native context management requires a model with safe summarization support")
		}
	}
	messages, cached, err := recorder.initialMessages(policy.Enabled)
	if err != nil {
		return nil, err
	}
	if cached != nil {
		return cached, nil
	}
	result = &nativeExecutionResult{Messages: append([]NativeMessage(nil), messages...), Usage: recorder.state.Usage}
	progress := result
	defer func() {
		if execErr != nil {
			result = progress
			return
		}
		if err := recorder.save(ctx, "done", progress); err != nil {
			execErr = err
		}
	}()
	phase := "ready"
	if len(nativeApprovalPlaceholders(messages)) > 0 {
		phase = "approval_tools"
	}
	if err := nativeBudgetCheck(ctx, execCtx, NativeContextPolicy{}, result.Usage, 0, 0); err != nil {
		return result, err
	}
	if err := recorder.save(ctx, phase, result, map[string]any{"kind": "execution_start", "messages": messages}); err != nil {
		return result, err
	}
	// Reconcile any recorded pending-approval tool calls (execute approved ones,
	// deliver change-requests) before the model sees the transcript, so an
	// approved mutating call executes instead of being re-gated into a loop.
	messages, err = nativeReconcileResumedApprovals(ctx, execCtx, messages)
	result.Messages = append([]NativeMessage(nil), messages...)
	if err != nil {
		return result, err
	}
	if err := recorder.save(ctx, "ready", result); err != nil {
		return result, err
	}
	systemPrompt := nativeSystemPrompt(execCtx)
	completionCorrections := 0
	maxCompletionCorrections := turnCompletionMaxCorrections(execCtx)
	overflowRetried := false
	nextCompactionAttempt := 0

	for step := 0; step < maxSteps; step++ {
		currentInput := nativeRequestTokens(systemPrompt, result.Messages, definitions) + recorder.state.InputAdjustment
		if currentInput >= nextCompactionAttempt {
			if _, err := nativeCompact(ctx, recorder, model, policy, systemPrompt, definitions, result, false); err != nil {
				if ctx.Err() != nil || currentInput >= policy.InputLimit || errors.Is(err, errNativeCheckpoint) {
					return result, err
				}
				// Preserve a usable context on a soft-threshold summary failure, but
				// do not buy another summary on every following tool round.
				slog.WarnContext(ctx, "native compaction deferred", "run_id", execCtx.Run.ID, "error", err)
				nextCompactionAttempt = currentInput + max(1024, policy.TriggerTokens/8)
			}
		}
		messages = result.Messages
		estimatedInput := nativeRequestTokens(systemPrompt, messages, definitions)
		inputTokens := estimatedInput + recorder.state.InputAdjustment
		if policy.Enabled && inputTokens > policy.InputLimit {
			return result, fmt.Errorf("native request exceeds configured input limit after compaction")
		}
		outputTokens := defaultNativeMaxTokens
		if policy.Enabled {
			outputTokens = policy.MaxOutputTokens
		}
		if err := nativeBudgetCheck(ctx, execCtx, policy, result.Usage, inputTokens, outputTokens); err != nil {
			return result, err
		}
		if err := recorder.save(ctx, "model", result); err != nil {
			return result, err
		}
		emitNativeEvent(ctx, execCtx, "context.usage", map[string]any{"estimated_input_tokens": inputTokens, "generation": recorder.state.Generation, "step": step})
		response, assistantMessageID, err := generateNativeModelResponse(ctx, execCtx, model, NativeModelRequest{
			SystemPrompt: systemPrompt,
			Messages:     append([]NativeMessage(nil), messages...),
			Tools:        append([]tools.Definition(nil), definitions...),
			Step:         step,
		})
		if err != nil {
			if response != nil {
				if saveErr := recorder.checkpointUsage(ctx, result, response, "failed_response"); saveErr != nil {
					return result, saveErr
				}
			}
			if policy.Enabled && !overflowRetried && nativeContextOverflow(err) {
				overflowRetried = true
				if compacted, compactErr := nativeCompact(ctx, recorder, model, policy, systemPrompt, definitions, result, true); compactErr == nil && compacted {
					continue
				}
			}
			return result, err
		}
		if response == nil {
			return nil, fmt.Errorf("native model returned nil response")
		}
		assistant := normalizeNativeAssistantMessage(response.Message)
		if strings.TrimSpace(assistantMessageID) == "" {
			assistantMessageID = emitNativeAssistantMessage(ctx, execCtx, assistant)
		}
		result.AssistantMessageID = assistantMessageID
		result.AssistantText = nativeMessageText(assistant)
		if response.Continuation != nil {
			result.Continuation = response.Continuation
		}
		messages = append(messages, assistant)
		result.Messages = append(result.Messages, assistant)
		if response.Usage.InputTokens > 0 {
			recorder.state.InputAdjustment = max(0, int(response.Usage.InputTokens)-estimatedInput)
		}
		if err := recorder.checkpointUsage(ctx, result, response, "agent"); err != nil {
			return result, err
		}
		overflowRetried = false

		toolCalls := nativeToolCallBlocks(assistant)
		if len(toolCalls) > 0 {
			if err := nativeBudgetCheck(ctx, execCtx, NativeContextPolicy{}, result.Usage, 0, 0); err != nil {
				return result, err
			}
		}
		if len(toolCalls) == 0 {
			if explicitTurnCompletionEnabled(execCtx) {
				if completionCorrections >= maxCompletionCorrections {
					return nil, turnCompletionGuardExhausted(maxCompletionCorrections, "the model ended its response without finish_turn")
				}
				completionCorrections++
				result.CompletionCorrections = completionCorrections
				correction := nativeTurnCompletionCorrection(completionCorrections, maxCompletionCorrections, "the response ended without finish_turn")
				messages = append(messages, correction)
				result.Messages = append(result.Messages, correction)
				continue
			}
			return result, nil
		}
		if explicitTurnCompletionEnabled(execCtx) {
			assistantText := nativeMessageText(assistant)
			for i := range toolCalls {
				if tools.CanonicalName(toolCalls[i].ToolName) == nativeToolFinishTurn {
					toolCalls[i].FinishAssistantText = assistantText
					toolCalls[i].FinishRejected = len(toolCalls) != 1
				}
			}
		}
		finishRejected := false
		for _, executed := range executeNativeToolCallsForRound(ctx, execCtx, toolCalls, assistantMessageID) {
			summary := truncateNativeText(executed.Output, nativeToolSummaryLimit)
			errorText := ""
			if executed.IsError {
				errorText = summary
			}
			result.ToolSummaries = append(result.ToolSummaries, nativeToolSummary{
				ID:         executed.ToolCallID,
				Name:       executed.ToolName,
				Summary:    summary,
				Error:      errorText,
				DurationMs: executed.Duration.Milliseconds(),
			})
			status := "completed"
			if executed.IsError {
				status = "failed"
			}
			result.ToolInvocations = append(result.ToolInvocations, nativeToolInvocation{
				ToolCallID:    executed.ToolCallID,
				ToolName:      executed.ToolName,
				Input:         append(json.RawMessage(nil), executed.Input...),
				OutputSummary: summary,
				DurationMs:    executed.Duration.Milliseconds(),
				Status:        status,
				Error:         errorText,
			})
			toolMessage := NativeMessage{
				Role:    "tool",
				Content: executed.Output,
				Blocks: []NativeBlock{{
					Type:       nativeBlockTypeToolResult,
					ToolCallID: executed.ToolCallID,
					ToolName:   executed.ToolName,
					Input:      append(json.RawMessage(nil), executed.Input...),
					Output:     executed.Output,
					IsError:    executed.IsError,
				}},
			}
			messages = append(messages, toolMessage)
			result.Messages = append(result.Messages, toolMessage)
			recordNativeToolCall(ctx, execCtx, executed, summary, errorText)
			if executed.ToolName == nativeToolFinishTurn {
				if executed.IsError {
					finishRejected = true
				} else if executed.TurnFinished {
					result.TurnFinished = true
					result.TurnOutcome = executed.TurnOutcome
					if strings.TrimSpace(result.AssistantText) == "" {
						result.AssistantText = executed.FinishSummary
					}
					if executed.TurnOutcome == "blocked" {
						result.AwaitingInput = true
					}
				}
			}
			switch executed.PauseReason {
			case agentcore.PauseReasonHumanInput:
				result.AwaitingInput = true
			case agentcore.PauseReasonHumanApproval:
				result.AwaitingApproval = true
			}
			if err := recorder.save(ctx, "tools", result, map[string]any{"kind": "tool_result", "message": toolMessage}); err != nil {
				return result, err
			}
			if result.AwaitingInput || result.AwaitingApproval {
				return result, nil
			}
		}
		if result.TurnFinished {
			return result, nil
		}
		if finishRejected {
			if completionCorrections >= maxCompletionCorrections {
				return nil, turnCompletionGuardExhausted(maxCompletionCorrections, "the model called finish_turn incorrectly")
			}
			completionCorrections++
			result.CompletionCorrections = completionCorrections
			correction := nativeTurnCompletionCorrection(completionCorrections, maxCompletionCorrections, "finish_turn was invalid; inspect its tool result")
			messages = append(messages, correction)
			result.Messages = append(result.Messages, correction)
		}
		if err := recorder.save(ctx, "ready", result); err != nil {
			return result, err
		}
	}
	result.MaxSteps = true
	return result, fmt.Errorf("native runtime reached max tool steps")
}

func nativeMaxToolSteps(execCtx *ExecutionContext, configuredDefault int) int {
	maxSteps := configuredDefault
	if maxSteps <= 0 {
		maxSteps = defaultNativeMaxToolSteps
	}
	if execCtx == nil || execCtx.Agent == nil || len(execCtx.Agent.ExecutionConfig) == 0 {
		return maxSteps
	}
	var executionConfig struct {
		MaxToolSteps int `json:"max_tool_steps"`
	}
	if err := json.Unmarshal(execCtx.Agent.ExecutionConfig, &executionConfig); err != nil || executionConfig.MaxToolSteps <= 0 {
		return maxSteps
	}
	if executionConfig.MaxToolSteps > maximumNativeMaxToolSteps {
		return maximumNativeMaxToolSteps
	}
	return executionConfig.MaxToolSteps
}

func generateNativeModelResponse(ctx context.Context, execCtx *ExecutionContext, model NativeModel, req NativeModelRequest) (*NativeModelResponse, string, error) {
	if streaming, ok := model.(NativeStreamingModel); ok {
		stream, err := streaming.Stream(ctx, req)
		if err != nil {
			return nil, "", err
		}
		if stream != nil {
			response, messageID, err := collectNativeModelStream(ctx, execCtx, stream)
			return response, messageID, err
		}
	}
	response, err := model.Generate(ctx, req)
	return response, "", err
}

func collectNativeModelStream(ctx context.Context, execCtx *ExecutionContext, stream NativeModelStream) (*NativeModelResponse, string, error) {
	defer stream.Close()
	messageID := uuid.NewString()
	reasoningMessageID := uuid.NewString()
	emitNativeEvent(ctx, execCtx, "assistant_message_started", map[string]any{"message_id": messageID})

	var chunks []NativeModelResponse
	var usage NativeUsage
	reasoningStarted := false
	assistantText := ""
	reasoningText := ""
	assistantDeltaEvents := 0
	reasoningDeltaEvents := 0
	toolCallStartedEvents := 0
	toolArgs := map[string]string{}
	toolStarted := map[string]bool{}
	currentToolCallID := ""
	currentToolName := ""
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return &NativeModelResponse{Usage: usage}, messageID, err
		}
		if chunk == nil {
			continue
		}
		chunks = append(chunks, *chunk)
		usage = maxNativeUsage(usage, chunk.Usage)
		message := normalizeNativeAssistantMessage(chunk.Message)
		// Read text from the raw chunk: normalize synthesizes Content by
		// trimming block text, which destroys inter-token whitespace.
		if text := nativeStreamMessageText(chunk.Message); text != "" {
			text = nativeStreamDelta(assistantText, text)
			if text != "" {
				assistantText += text
				emitNativeEvent(ctx, execCtx, "assistant_message_delta", map[string]any{
					"message_id": messageID,
					"text":       text,
					"content":    text,
				})
				assistantDeltaEvents++
			}
		}
		if reasoning := message.ReasoningContent; reasoning != "" {
			if !reasoningStarted {
				reasoningStarted = true
				emitNativeEvent(ctx, execCtx, "reasoning_message_started", map[string]any{"message_id": reasoningMessageID})
			}
			reasoning = nativeStreamDelta(reasoningText, reasoning)
			if reasoning != "" {
				reasoningText += reasoning
				emitNativeEvent(ctx, execCtx, "reasoning_message_delta", map[string]any{
					"message_id": reasoningMessageID,
					"text":       reasoning,
					"content":    reasoning,
				})
				reasoningDeltaEvents++
			}
		}
		for _, toolCall := range nativeRawToolCallBlocks(message) {
			toolCallID := strings.TrimSpace(toolCall.ToolCallID)
			toolName := tools.CanonicalName(toolCall.ToolName)
			if toolCallID == "" && toolName == "" && currentToolCallID != "" {
				toolCallID = currentToolCallID
				toolName = currentToolName
			}
			if toolCallID == "" {
				if toolName == "" {
					continue
				}
				toolCallID = uuid.NewString()
			}
			if toolName != "" {
				currentToolCallID = toolCallID
				currentToolName = toolName
			} else if currentToolName != "" && toolCallID == currentToolCallID {
				toolName = currentToolName
			}
			if toolName == "" {
				continue
			}
			// Keep argument fragments verbatim: streamed JSON splits at token
			// boundaries, so trimming each fragment deletes spaces inside
			// string values ("Choose conversion goal" -> "Chooseconversiongoal").
			argsDelta := string(toolCall.Input)
			if strings.TrimSpace(argsDelta) == "{}" {
				argsDelta = ""
			}
			if !toolStarted[toolCallID] {
				toolStarted[toolCallID] = true
				emitNativeEvent(ctx, execCtx, "tool_call_started", map[string]any{
					"tool_call_id":      toolCallID,
					"tool_name":         toolName,
					"tool_input":        truncateNativeText(strings.TrimSpace(argsDelta), 200),
					"parent_message_id": messageID,
					"args_text":         argsDelta,
				})
				toolCallStartedEvents++
			}
			if argsDelta != "" {
				toolArgs[toolCallID] += argsDelta
				emitNativeEvent(ctx, execCtx, "tool_call_args_delta", map[string]any{
					"tool_call_id":      toolCallID,
					"tool_name":         toolName,
					"parent_message_id": messageID,
					"args_delta":        argsDelta,
					"args_text":         toolArgs[toolCallID],
				})
			}
		}
	}
	response := concatNativeModelStreamResponses(chunks)
	response.Usage = maxNativeUsage(response.Usage, usage)
	text := nativeMessageText(response.Message)
	emitNativeEvent(ctx, execCtx, "assistant_message_completed", map[string]any{
		"message_id": messageID,
		"text":       text,
		"content":    text,
	})
	if reasoningStarted {
		reasoning := strings.TrimSpace(response.Message.ReasoningContent)
		emitNativeEvent(ctx, execCtx, "reasoning_message_completed", map[string]any{
			"message_id": reasoningMessageID,
			"text":       reasoning,
			"content":    reasoning,
		})
	}
	var appID, runID, runtimeKind string
	if execCtx != nil {
		appID = execCtx.AppID
		if execCtx.Run != nil {
			runID = execCtx.Run.ID
			runtimeKind = execCtx.Run.RuntimeKind
		}
	}
	slog.DebugContext(ctx, "native stream event counts",
		"app_id", appID,
		"run_id", runID,
		"runtime_kind", runtimeKind,
		"assistant_message_delta", assistantDeltaEvents,
		"reasoning_message_delta", reasoningDeltaEvents,
		"tool_call_started", toolCallStartedEvents,
	)
	return response, messageID, nil
}

func concatNativeModelStreamResponses(chunks []NativeModelResponse) *NativeModelResponse {
	response := &NativeModelResponse{Message: NativeMessage{Role: "assistant"}}
	if len(chunks) == 0 {
		return response
	}
	type toolAccumulator struct {
		id     string
		name   string
		input  strings.Builder
		source NativeBlock
	}
	var text strings.Builder
	var reasoning strings.Builder
	toolsByID := map[string]*toolAccumulator{}
	var toolOrder []string
	currentToolID := ""
	for _, chunk := range chunks {
		// Use the raw chunk: normalize synthesizes Content from trimmed block
		// text, which strips the inter-token whitespace we must preserve.
		message := chunk.Message
		if message.Content != "" {
			text.WriteString(nativeStreamDelta(text.String(), message.Content))
		} else {
			for _, block := range message.Blocks {
				if strings.TrimSpace(block.Type) == nativeBlockTypeText {
					text.WriteString(nativeStreamDelta(text.String(), block.Text))
				}
			}
		}
		reasoning.WriteString(nativeStreamDelta(reasoning.String(), message.ReasoningContent))
		for _, block := range nativeRawToolCallBlocks(message) {
			id := strings.TrimSpace(block.ToolCallID)
			name := tools.CanonicalName(block.ToolName)
			if id == "" && name == "" && currentToolID != "" {
				id = currentToolID
			}
			if id == "" {
				if name == "" {
					continue
				}
				id = fmt.Sprintf("tool-%d", len(toolOrder)+1)
			}
			acc := toolsByID[id]
			if acc == nil {
				acc = &toolAccumulator{id: id, source: block}
				toolsByID[id] = acc
				toolOrder = append(toolOrder, id)
			}
			if name != "" {
				acc.name = name
				currentToolID = id
			}
			appendNativeToolInputFragment(&acc.input, block.Input)
		}
		response.Usage = maxNativeUsage(response.Usage, chunk.Usage)
		if chunk.Continuation != nil {
			response.Continuation = chunk.Continuation
		}
	}
	response.Message.Content = strings.TrimSpace(text.String())
	response.Message.ReasoningContent = strings.TrimSpace(reasoning.String())
	if response.Message.Content != "" {
		response.Message.Blocks = append(response.Message.Blocks, NativeBlock{Type: nativeBlockTypeText, Text: response.Message.Content})
	}
	for _, id := range toolOrder {
		acc := toolsByID[id]
		name := firstNonEmpty(acc.name, tools.CanonicalName(acc.source.ToolName))
		if name == "" {
			continue
		}
		input := strings.TrimSpace(acc.input.String())
		if input == "" {
			input = "{}"
		}
		response.Message.Blocks = append(response.Message.Blocks, NativeBlock{
			Type:       nativeBlockTypeToolCall,
			ToolCallID: acc.id,
			ToolName:   name,
			Input:      normalizeNativeToolInput(json.RawMessage(input)),
		})
	}
	return response
}

func appendNativeToolInputFragment(builder *strings.Builder, raw json.RawMessage) {
	if builder == nil {
		return
	}
	fragment := string(raw)
	trimmed := strings.TrimSpace(fragment)
	if trimmed == "" || trimmed == "{}" {
		return
	}
	// Write the fragment verbatim; trimming eats spaces inside JSON string
	// values when the stream splits mid-string.
	builder.WriteString(fragment)
}

func nativeStreamDelta(current, incoming string) string {
	if incoming == "" {
		return ""
	}
	// Some providers stream cumulative snapshots rather than increments.
	if current != "" && strings.HasPrefix(incoming, current) {
		return incoming[len(current):]
	}
	// Incremental deltas must be appended verbatim: token boundaries fall
	// mid-word and provider deltas carry their own leading whitespace, so
	// any guessed spacing corrupts the text.
	return incoming
}

func maxNativeUsage(a, b NativeUsage) NativeUsage {
	if b.InputTokens > a.InputTokens {
		a.InputTokens = b.InputTokens
	}
	if b.CachedInputTokens > a.CachedInputTokens {
		a.CachedInputTokens = b.CachedInputTokens
	}
	if b.OutputTokens > a.OutputTokens {
		a.OutputTokens = b.OutputTokens
	}
	if b.ReasoningOutputTokens > a.ReasoningOutputTokens {
		a.ReasoningOutputTokens = b.ReasoningOutputTokens
	}
	return a
}

func nativeAllowedToolDefinitions(execCtx *ExecutionContext) []tools.Definition {
	if execCtx == nil {
		return nil
	}
	var definitions []tools.Definition
	if execCtx.Tools != nil && len(execCtx.AllowedTools) > 0 {
		definitions = execCtx.Tools.DefinitionsForApp(execCtx.AppID)
	}
	out := make([]tools.Definition, 0, len(definitions)+1)
	for _, def := range definitions {
		name := tools.CanonicalName(def.Name)
		if explicitTurnCompletionEnabled(execCtx) && name == nativeToolFinishTurn {
			// finish_turn is reserved by the runtime while this contract is active.
			continue
		}
		if execCtx.AllowedTools[name] {
			def.Name = name
			out = append(out, def)
		}
	}
	out = append(out, nativeAllowedInteractionToolDefinitions(execCtx, out)...)
	if explicitTurnCompletionEnabled(execCtx) {
		out = append(out, nativeFinishTurnToolDefinition())
	}
	return out
}

// nativeTranscriptGuidance is appended to every native-SDK system prompt.
// The engine's assistant-text events are the transcript hosts render; some
// models (notably OpenAI reasoning models) emit tool-call-only turns unless
// explicitly told to narrate, which leaves the transcript empty.
const nativeTranscriptGuidance = "Transcript communication: your plain-text assistant messages are the run transcript shown to the human. Before a tool call or batch of related tool calls, write one short sentence saying what you are doing and why. Report important findings and decisions as brief text updates as the run progresses. Do not work through tool calls silently with no accompanying text."

func nativeSystemPrompt(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.Agent == nil {
		return ""
	}
	parts := []string{strings.TrimSpace(execCtx.Agent.SystemPrompt), nativeTranscriptGuidance}
	if explicitTurnCompletionEnabled(execCtx) {
		parts = append(parts, nativeTurnCompletionInstructions())
	}
	if strings.TrimSpace(execCtx.SkillInstructions) != "" {
		parts = append(parts, "Skill instructions:\n"+strings.TrimSpace(execCtx.SkillInstructions))
	}
	if execCtx.TargetContext != nil && strings.TrimSpace(execCtx.TargetContext.Summary) != "" {
		parts = append(parts, "Target context:\n"+strings.TrimSpace(execCtx.TargetContext.Summary))
	}
	if workspaceContext := nativeWorkspaceContext(execCtx); workspaceContext != "" {
		parts = append(parts, workspaceContext)
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n\n")
}

func nativeWorkspaceContext(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.WorkspaceLease == nil {
		return ""
	}
	lease := execCtx.WorkspaceLease
	repository := strings.TrimSpace(nativeMetadataString(lease.Metadata, "repo_full_name"))
	baseBranch := strings.TrimSpace(nativeMetadataString(lease.Metadata, "base_branch"))
	workBranch := strings.TrimSpace(nativeMetadataString(lease.Metadata, "work_branch"))
	lines := []string{"Repository workspace: a checkout is already prepared for this run. Use the current filesystem workspace directly. Do not call repository discovery or checkout tools, and do not ask the human which repository to use, unless a filesystem tool explicitly reports that the checkout is unavailable."}
	if repository != "" {
		lines = append(lines, "Repository: "+repository)
	}
	if baseBranch != "" {
		lines = append(lines, "Base branch: "+baseBranch)
	}
	if workBranch != "" {
		lines = append(lines, "Working branch: "+workBranch)
	}
	return strings.Join(lines, "\n")
}

func nativeMetadataString(metadata map[string]interface{}, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	value, _ := metadata[key].(string)
	return value
}

func nativeInitialUserPrompt(execCtx *ExecutionContext) string {
	if execCtx == nil || execCtx.Run == nil {
		return "Run the agent task."
	}
	parts := []string{strings.TrimSpace(execCtx.Run.Input.Instructions)}
	if execCtx.TargetContext != nil && strings.TrimSpace(execCtx.TargetContext.Summary) != "" {
		parts = append(parts, "Context:\n"+strings.TrimSpace(execCtx.TargetContext.Summary))
	}
	if len(parts) == 0 || strings.TrimSpace(strings.Join(parts, "")) == "" {
		return fmt.Sprintf("Run the agent task for %s/%s.", execCtx.Run.Target.Type, execCtx.Run.Target.ID)
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, "\n\n")
}

func normalizeNativeAssistantMessage(message NativeMessage) NativeMessage {
	message.Role = strings.TrimSpace(message.Role)
	if message.Role == "" {
		message.Role = "assistant"
	}
	if len(message.Blocks) == 0 && strings.TrimSpace(message.Content) != "" {
		message.Blocks = []NativeBlock{{Type: nativeBlockTypeText, Text: message.Content}}
	}
	if strings.TrimSpace(message.Content) == "" {
		message.Content = nativeMessageText(message)
	}
	return message
}

func nativeMessageText(message NativeMessage) string {
	parts := make([]string, 0, len(message.Blocks)+1)
	if strings.TrimSpace(message.Content) != "" {
		parts = append(parts, strings.TrimSpace(message.Content))
	}
	for _, block := range message.Blocks {
		if strings.TrimSpace(block.Type) == nativeBlockTypeText && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[len(parts)-1])
}

func nativeStreamMessageText(message NativeMessage) string {
	if message.Content != "" {
		return message.Content
	}
	for _, block := range message.Blocks {
		if strings.TrimSpace(block.Type) == nativeBlockTypeText && block.Text != "" {
			return block.Text
		}
	}
	return ""
}

func nativeToolCallBlocks(message NativeMessage) []NativeBlock {
	var out []NativeBlock
	for _, block := range nativeRawToolCallBlocks(message) {
		block.ToolName = tools.CanonicalName(block.ToolName)
		if block.ToolName == "" {
			continue
		}
		if block.ToolCallID == "" {
			block.ToolCallID = uuid.NewString()
		}
		if len(block.Input) == 0 {
			block.Input = json.RawMessage(`{}`)
		}
		out = append(out, block)
	}
	return out
}

func nativeRawToolCallBlocks(message NativeMessage) []NativeBlock {
	var out []NativeBlock
	for _, block := range message.Blocks {
		if strings.TrimSpace(block.Type) != nativeBlockTypeToolCall {
			continue
		}
		out = append(out, block)
	}
	return out
}

func emitNativeAssistantMessage(ctx context.Context, execCtx *ExecutionContext, message NativeMessage) string {
	messageID := uuid.NewString()
	text := nativeMessageText(message)
	emitNativeEvent(ctx, execCtx, "assistant_message_started", map[string]any{"message_id": messageID})
	if text != "" {
		emitNativeEvent(ctx, execCtx, "assistant_message_delta", map[string]any{
			"message_id": messageID,
			"text":       text,
			"content":    text,
		})
	}
	emitNativeEvent(ctx, execCtx, "assistant_message_completed", map[string]any{
		"message_id": messageID,
		"text":       text,
		"content":    text,
	})
	return messageID
}

func executeNativeToolCallsForRound(ctx context.Context, execCtx *ExecutionContext, toolCalls []NativeBlock, parentMessageID string) []nativeExecutedToolCall {
	if len(toolCalls) == 0 {
		return nil
	}
	emitStarted := func(toolCall NativeBlock) {
		argsText := strings.TrimSpace(string(normalizeNativeToolInput(toolCall.Input)))
		emitNativeEvent(ctx, execCtx, "tool_call_started", map[string]any{
			"tool_call_id":      strings.TrimSpace(toolCall.ToolCallID),
			"tool_name":         strings.TrimSpace(toolCall.ToolName),
			"tool_input":        truncateNativeText(argsText, 200),
			"parent_message_id": strings.TrimSpace(parentMessageID),
			"args_text":         argsText,
		})
		if argsText != "" {
			emitNativeEvent(ctx, execCtx, "tool_call_args_delta", map[string]any{
				"tool_call_id":      strings.TrimSpace(toolCall.ToolCallID),
				"tool_name":         strings.TrimSpace(toolCall.ToolName),
				"parent_message_id": strings.TrimSpace(parentMessageID),
				"args_delta":        argsText,
				"args_text":         argsText,
			})
		}
	}
	emitFinished := func(executed nativeExecutedToolCall) {
		resultMessageID := uuid.NewString()
		errorText := ""
		if executed.IsError {
			errorText = truncateNativeText(executed.Output, nativeToolSummaryLimit)
		}
		emitNativeEvent(ctx, execCtx, "tool_call_result", map[string]any{
			"tool_call_id":      strings.TrimSpace(executed.ToolCallID),
			"tool_name":         strings.TrimSpace(executed.ToolName),
			"parent_message_id": strings.TrimSpace(parentMessageID),
			"result_message_id": resultMessageID,
			"content":           truncateNativeText(executed.Output, nativeToolEventLimit),
			"output_summary":    truncateNativeText(executed.Output, nativeToolSummaryLimit),
			"error":             errorText,
		})
		emitNativeEvent(ctx, execCtx, "tool_call_finished", map[string]any{
			"tool_call_id":      strings.TrimSpace(executed.ToolCallID),
			"tool_name":         strings.TrimSpace(executed.ToolName),
			"parent_message_id": strings.TrimSpace(parentMessageID),
			"result_message_id": resultMessageID,
			"output_summary":    truncateNativeText(executed.Output, nativeToolSummaryLimit),
			"content":           truncateNativeText(executed.Output, nativeToolEventLimit),
			"duration_ms":       executed.Duration.Milliseconds(),
			"error":             errorText,
		})
	}

	results := make([]nativeExecutedToolCall, 0, len(toolCalls))
	if !canExecuteNativeToolCallsInParallel(execCtx, toolCalls) {
		for _, toolCall := range toolCalls {
			emitStarted(toolCall)
			executed := executeSingleNativeToolCall(ctx, execCtx, toolCall)
			results = append(results, executed)
			emitFinished(executed)
			if executed.PauseReason != "" {
				break
			}
		}
		return results
	}

	results = make([]nativeExecutedToolCall, len(toolCalls))
	var wg sync.WaitGroup
	var eventMu sync.Mutex
	wg.Add(len(toolCalls))
	for i, toolCall := range toolCalls {
		go func(index int, pending NativeBlock) {
			defer wg.Done()
			eventMu.Lock()
			emitStarted(pending)
			eventMu.Unlock()

			executed := executeSingleNativeToolCall(ctx, execCtx, pending)
			results[index] = executed

			eventMu.Lock()
			emitFinished(executed)
			eventMu.Unlock()
		}(i, toolCall)
	}
	wg.Wait()
	return results
}

func executeSingleNativeToolCall(ctx context.Context, execCtx *ExecutionContext, toolCall NativeBlock) nativeExecutedToolCall {
	start := time.Now()
	name := tools.CanonicalName(toolCall.ToolName)
	if name == nativeToolFinishTurn && explicitTurnCompletionEnabled(execCtx) {
		executed := executeNativeFinishTurn(toolCall)
		executed.Duration = time.Since(start)
		return executed
	}
	if nativeIsInteractionTool(name) {
		executed := executeNativeInteractionTool(ctx, execCtx, toolCall)
		executed.Duration = time.Since(start)
		return executed
	}
	mutating := false
	def := tools.Definition{Name: name}
	if registered, ok := execCtx.Tools.DefinitionForApp(execCtx.AppID, name); ok {
		def = registered
		mutating = def.Mutating
	}
	input := normalizeNativeToolInput(toolCall.Input)
	if mutating && nativeRequiresApproval(execCtx, def) {
		output, interactionID, err := nativeRequestToolApproval(ctx, execCtx, def, input)
		executed := nativeExecutedToolCall{
			ToolCallID:       strings.TrimSpace(toolCall.ToolCallID),
			ToolName:         name,
			Input:            input,
			Output:           strings.TrimSpace(output),
			Duration:         time.Since(start),
			Mutating:         mutating,
			ApprovalRequired: true,
			PauseReason:      agentcore.PauseReasonHumanApproval,
			InteractionID:    interactionID,
		}
		if err != nil {
			executed.IsError = true
			executed.Output = err.Error()
			executed.PauseReason = ""
			executed.InteractionID = ""
		}
		return executed
	}
	output, err := execCtx.Tools.Execute(ctx, toolCallContext(execCtx), name, input)
	duration := time.Since(start)
	text := strings.TrimSpace(tools.ToolResultText(output))
	isError := err != nil
	if err != nil {
		text = err.Error()
	}
	return nativeExecutedToolCall{
		ToolCallID: strings.TrimSpace(toolCall.ToolCallID),
		ToolName:   name,
		Input:      input,
		Output:     text,
		Duration:   duration,
		IsError:    isError,
		Mutating:   mutating,
	}
}

func canExecuteNativeToolCallsInParallel(execCtx *ExecutionContext, toolCalls []NativeBlock) bool {
	if execCtx == nil || execCtx.Tools == nil || len(toolCalls) < 2 {
		return false
	}
	for _, toolCall := range toolCalls {
		def, ok := execCtx.Tools.DefinitionForApp(execCtx.AppID, toolCall.ToolName)
		if !ok || def.Mutating {
			return false
		}
	}
	return true
}

func recordNativeToolCall(ctx context.Context, execCtx *ExecutionContext, executed nativeExecutedToolCall, summary string, errorText string) {
	if execCtx == nil || execCtx.Store == nil || execCtx.Run == nil {
		return
	}
	output, _ := json.Marshal(map[string]any{
		"runtime_kind": agentcore.RuntimeNativeSDK,
		"tool_call_id": strings.TrimSpace(executed.ToolCallID),
		"summary":      strings.TrimSpace(summary),
		"error":        strings.TrimSpace(errorText),
		"duration_ms":  executed.Duration.Milliseconds(),
		"output":       executed.Output,
	})
	_ = execCtx.Store.AppendToolCall(ctx, &agentcore.ToolCall{
		AppID:            execCtx.Run.AppID,
		RunID:            execCtx.Run.ID,
		ToolName:         strings.TrimSpace(executed.ToolName),
		Input:            append(json.RawMessage(nil), executed.Input...),
		Output:           output,
		Error:            strings.TrimSpace(errorText),
		Mutating:         executed.Mutating,
		ApprovalRequired: executed.ApprovalRequired,
	})
}

func normalizeNativeToolInput(raw json.RawMessage) json.RawMessage {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	if json.Valid(raw) && len(raw) > 0 && raw[0] == '{' {
		return append(json.RawMessage(nil), raw...)
	}
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err == nil {
		decoded = strings.TrimSpace(decoded)
		if decoded == "" {
			return json.RawMessage(`{}`)
		}
		wrapped, _ := json.Marshal(map[string]string{"raw": decoded})
		return wrapped
	}
	wrapped, _ := json.Marshal(map[string]string{"raw": string(raw)})
	return wrapped
}

func emitNativeEvent(ctx context.Context, execCtx *ExecutionContext, eventType string, data map[string]any) {
	if execCtx == nil || execCtx.EventSink == nil || execCtx.Run == nil {
		return
	}
	execCtx.EventSink.Emit(ctx, Event{
		AppID: execCtx.Run.AppID,
		RunID: execCtx.Run.ID,
		Type:  eventType,
		Data:  data,
	})
}

func truncateNativeText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}
