package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type NativeAdapter struct {
	cfg NativeConfig
}

func NewNativeAdapter() *NativeAdapter {
	return NewNativeAdapterWithConfig(NativeConfig{})
}

func NewNativeAdapterWithConfig(cfg NativeConfig) *NativeAdapter {
	return &NativeAdapter{cfg: cfg}
}

func (a *NativeAdapter) Kind() string {
	return agentcore.RuntimeNativeSDK
}

func (a *NativeAdapter) Execute(execCtx *ExecutionContext) (*Result, error) {
	if execCtx == nil || execCtx.Run == nil || execCtx.Agent == nil {
		return nil, fmt.Errorf("execution context is incomplete")
	}
	if a.cfg.ModelFactory != nil {
		ctx := execCtx.Context
		if ctx == nil {
			ctx = context.Background()
		}
		execResult, err := executeNativeModel(ctx, execCtx, a.cfg)
		if err != nil {
			return nil, err
		}
		messagesPersisted, err := persistNativeRunMessages(ctx, execCtx, execResult)
		if err != nil {
			return nil, err
		}
		summary, _ := json.Marshal(map[string]interface{}{
			"runtime_kind":            agentcore.RuntimeNativeSDK,
			"target_type":             execCtx.Run.Target.Type,
			"target_id":               execCtx.Run.Target.ID,
			"input_tokens":            execResult.Usage.InputTokens,
			"cached_input_tokens":     execResult.Usage.CachedInputTokens,
			"output_tokens":           execResult.Usage.OutputTokens,
			"reasoning_output_tokens": execResult.Usage.ReasoningOutputTokens,
			"tool_summaries":          execResult.ToolSummaries,
			"provider_continuation":   execResult.Continuation,
			"max_tool_steps_reached":  execResult.MaxSteps,
			"native_messages":         execResult.Messages,
		})
		return &Result{
			AssistantMessage:  execResult.AssistantText,
			OutputSummary:     summary,
			WaitForApproval:   execResult.AwaitingApproval,
			AwaitingInput:     execResult.AwaitingInput,
			MessagesPersisted: messagesPersisted,
		}, nil
	}
	contextSummary := ""
	if execCtx.TargetContext != nil {
		contextSummary = strings.TrimSpace(execCtx.TargetContext.Summary)
	}
	if contextSummary == "" {
		contextSummary = fmt.Sprintf("%s %s", execCtx.Run.Target.Type, execCtx.Run.Target.ID)
	}
	toolContext := ""
	if execCtx.Tools != nil && execCtx.AllowedTools["get_context"] {
		if output, err := execCtx.Tools.Execute(execCtx.Context, toolCallContext(execCtx), "get_context", json.RawMessage(`{}`)); err == nil {
			toolContext = strings.TrimSpace(string(output))
		}
	}
	message := strings.TrimSpace(strings.Join([]string{
		fmt.Sprintf("Agent %s completed a native_sdk run.", execCtx.Agent.Name),
		"Target: " + execCtx.Run.Target.Type + "/" + execCtx.Run.Target.ID,
		"Context: " + contextSummary,
		"Tool context: " + toolContext,
		"Instructions: " + execCtx.Run.Input.Instructions,
	}, "\n"))
	summary, _ := json.Marshal(map[string]interface{}{
		"runtime_kind": agentcore.RuntimeNativeSDK,
		"target_type":  execCtx.Run.Target.Type,
		"target_id":    execCtx.Run.Target.ID,
	})
	return &Result{
		AssistantMessage: message,
		OutputSummary:    summary,
	}, nil
}

func toolCallContext(execCtx *ExecutionContext) tools.CallContext {
	if execCtx == nil || execCtx.Run == nil {
		return tools.CallContext{}
	}
	return tools.CallContext{
		AppID:           execCtx.AppID,
		RunID:           execCtx.Run.ID,
		Agent:           execCtx.Agent,
		Run:             execCtx.Run,
		Target:          execCtx.Run.Target,
		StagedSkillRoot: execCtx.StagedSkillRoot,
	}
}
