package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

var errNativeCheckpoint = errors.New("native checkpoint persistence failed")

type nativeCheckpoint struct {
	StartedCalls map[string]NativeBlock `json:"started_calls,omitempty"`
	Format       int                    `json:"format"`
	// loadedFormat is the format of the loaded checkpoint, or the current
	// format for a fresh recorder. Format 1 predates started-call markers, so
	// a missing marker there means nothing about whether a call launched.
	loadedFormat        int
	Managed             bool                   `json:"managed"`
	ResumeKey           string                 `json:"resume_key"`
	WorkspaceRecoveryID string                 `json:"workspace_recovery_id,omitempty"`
	Instructions        string                 `json:"instructions,omitempty"`
	InputAdjustment     int                    `json:"input_adjustment,omitempty"`
	NoProgress          *nativeNoProgressState `json:"no_progress,omitempty"`
	Phase               string                 `json:"phase"`
	Generation          int                    `json:"generation"`
	Messages            []NativeMessage        `json:"messages,omitempty"`
	Usage               NativeUsage            `json:"usage"`
	Result              *nativeExecutionResult `json:"result,omitempty"`
}

// nativeCheckpointFormat 2 adds started-call markers for mutating tools.
const nativeCheckpointFormat = 2

// markersRecorded reports whether a missing marker proves a call never launched.
func (c *nativeCheckpoint) markersRecorded() bool {
	return c.loadedFormat >= 2
}

type nativeRecorder struct {
	store   agentcore.NativeStateStore
	record  agentcore.NativeState
	state   nativeCheckpoint
	execCtx *ExecutionContext
}

func openNativeRecorder(ctx context.Context, execCtx *ExecutionContext, managed bool) (*nativeRecorder, error) {
	r := &nativeRecorder{execCtx: execCtx, record: agentcore.NativeState{AppID: execCtx.AppID, RunID: execCtx.Run.ID}}
	r.state.Format = nativeCheckpointFormat
	r.state.loadedFormat = nativeCheckpointFormat
	r.state.Usage = nativeUsageFromSummary(execCtx.Run.OutputSummary)
	r.store, _ = execCtx.Store.(agentcore.NativeStateStore)
	if r.store == nil {
		return nil, fmt.Errorf("native execution requires checkpoint storage")
	}
	if r.store != nil {
		stored, err := r.store.LoadNativeState(ctx, execCtx.AppID, execCtx.Run.ID)
		if err != nil {
			return nil, fmt.Errorf("load native checkpoint: %w", err)
		}
		if stored != nil {
			r.record = *stored
			if err := json.Unmarshal(stored.Payload, &r.state); err != nil || r.state.Format < 1 || r.state.Format > nativeCheckpointFormat {
				return nil, fmt.Errorf("unsupported or invalid native checkpoint")
			}
			r.state.loadedFormat = r.state.Format
			r.state.Format = nativeCheckpointFormat
			r.state.Usage = maxNativeUsage(r.state.Usage, nativeUsageFromSummary(execCtx.Run.OutputSummary))
		}
	}
	return r, nil
}

func nativeResumeKey(execCtx *ExecutionContext) string {
	var value any
	if execCtx.Run.Input.Metadata != nil {
		value = execCtx.Run.Input.Metadata["last_resume"]
	}
	body, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

func (r *nativeRecorder) initialMessages(managed bool) ([]NativeMessage, *nativeExecutionResult, error) {
	resumeKey := nativeResumeKey(r.execCtx)
	r.state.Managed = managed
	if len(r.state.Messages) > 0 {
		if r.state.Phase == "approval_tools" {
			if err := r.recoverApprovedTools(); err != nil {
				return nil, nil, err
			}
		}
		if r.state.Phase == "tools" {
			if err := r.recoverTools(); err != nil {
				return nil, nil, err
			}
		}
		r.reconcileWorkspaceRecovery()
		if r.state.ResumeKey == resumeKey && r.state.Instructions == r.execCtx.Run.Input.Instructions && r.state.Phase == "done" && r.state.Result != nil {
			r.state.Result.Usage = r.state.Usage
			r.state.Result.Messages = r.state.Messages
			return nil, r.state.Result, nil
		}
		messages := append([]NativeMessage(nil), r.state.Messages...)
		if r.state.ResumeKey == resumeKey && r.state.Instructions != r.execCtx.Run.Input.Instructions {
			messages = append(messages, NativeMessage{Role: "user", Content: r.execCtx.Run.Input.Instructions, Provenance: "host_request"})
			r.state.NoProgress = nil
		}
		if r.state.ResumeKey != resumeKey {
			r.state.NoProgress = nil
			if resume, ok := nativeLastResumePayload(r.execCtx); ok {
				messages = append(messages, nativeResumeMessage(resume))
			}
		}
		r.state.ResumeKey = resumeKey
		r.state.Instructions = r.execCtx.Run.Input.Instructions
		r.state.InputAdjustment = 0
		// Disabling auto-compaction never resurrects the discarded prefix.
		return messages, nil, nil
	}
	r.state.Managed = managed
	r.state.ResumeKey = resumeKey
	r.state.Instructions = r.execCtx.Run.Input.Instructions
	r.state.Messages = nativeInitialMessages(r.execCtx)
	r.reconcileWorkspaceRecovery()
	return r.state.Messages, nil, nil
}

func (r *nativeRecorder) save(ctx context.Context, phase string, result *nativeExecutionResult, entries ...any) error {
	r.state.Usage = result.Usage
	r.state.Phase = phase
	r.state.Result = nil
	r.state.Messages = result.Messages
	if phase == "done" || phase == "tools" {
		copy := *result
		copy.Messages = nil // Already stored once in the active checkpoint.
		r.state.Result = &copy
	}
	if r.store == nil {
		return nil
	}
	payload, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	var records []json.RawMessage
	for _, entry := range entries {
		body, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		records = append(records, body)
	}
	r.record.Payload = payload
	if err := r.store.SaveNativeState(ctx, &r.record, records); err != nil {
		return fmt.Errorf("%w: %w", errNativeCheckpoint, err)
	}
	return nil
}

func (r *nativeRecorder) checkpointUsage(ctx context.Context, result *nativeExecutionResult, response *NativeModelResponse, purpose string) error {
	// Provider work may have been billed before cancellation/stream failure.
	// Persist observed usage with a bounded cleanup context, without admitting
	// another model request or tool action.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	result.Usage = addNativeUsage(result.Usage, response.Usage)
	phase := r.state.Phase
	if purpose == "agent" && len(nativeToolCallBlocks(response.Message)) > 0 {
		phase = "tools"
	}
	if err := r.save(ctx, phase, result, map[string]any{"kind": "model_response", "purpose": purpose, "response": response}); err != nil {
		return err
	}
	emitNativeEvent(ctx, r.execCtx, "usage.checkpoint", map[string]any{"usage": nativeUsageMap(result.Usage), "usage_semantic": "cumulative"})
	return nil
}

func addNativeUsage(a, b NativeUsage) NativeUsage {
	return NativeUsage{InputTokens: a.InputTokens + b.InputTokens, CachedInputTokens: a.CachedInputTokens + b.CachedInputTokens, OutputTokens: a.OutputTokens + b.OutputTokens, ReasoningOutputTokens: a.ReasoningOutputTokens + b.ReasoningOutputTokens}
}

func nativeUsageFromSummary(raw json.RawMessage) NativeUsage {
	var usage NativeUsage
	_ = json.Unmarshal(raw, &usage)
	return usage
}

func nativeUsageMap(usage NativeUsage) map[string]any {
	return map[string]any{"input_tokens": usage.InputTokens, "cached_input_tokens": usage.CachedInputTokens, "output_tokens": usage.OutputTokens, "reasoning_output_tokens": usage.ReasoningOutputTokens, "total_tokens": usage.InputTokens + usage.OutputTokens}
}

// NativeCheckpointUsage recovers durable usage even if execution returned an error.
func NativeCheckpointUsage(ctx context.Context, store agentcore.Store, appID, runID string) (json.RawMessage, error) {
	states, ok := store.(agentcore.NativeStateStore)
	if !ok {
		return nil, nil
	}
	record, err := states.LoadNativeState(ctx, appID, runID)
	if err != nil || record == nil {
		return nil, err
	}
	var state nativeCheckpoint
	if err := json.Unmarshal(record.Payload, &state); err != nil {
		return nil, err
	}
	body := nativeUsageMap(state.Usage)
	body["usage_semantic"] = "cumulative"
	return json.Marshal(body)
}
