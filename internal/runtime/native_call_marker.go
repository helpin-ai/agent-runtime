package runtime

import (
	"context"
	"encoding/json"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const nativeUnknownOutcome = "Operation started, outcome unknown: execution was interrupted before a durable result was saved. External effects may have completed. Report this uncertainty to the user. Inspect the remote state before any retry; do not repeat a push, PR, POST, or other mutation merely to reconstruct a missing result."

type nativeCallRecorderKey struct{}
type nativeCallRecorder struct {
	recorder *nativeRecorder
	result   *nativeExecutionResult
	failed   error
}

func nativeNeedsOutcomeMarker(name string) bool {
	switch tools.CanonicalName(name) {
	case "run_command", "run_python", "commit_and_push", "create_pull_request", "open_pr":
		return true
	}
	return false
}

// Persist before launch, after authorization/approval. A failed checkpoint
// prevents launch. This is an uncertainty marker, not an exactly-once ledger.
func nativeMarkCallStarted(ctx context.Context, call NativeBlock) error {
	if !nativeNeedsOutcomeMarker(call.ToolName) {
		return nil
	}
	recording, ok := ctx.Value(nativeCallRecorderKey{}).(*nativeCallRecorder)
	if !ok {
		return nil
	} // Direct helper/CLI tests without a model checkpoint.
	r := recording.recorder
	if err := nativeBudgetCheck(ctx, r.execCtx, NativeContextPolicy{}, NativeUsage{}, 0, 0); err != nil {
		return err
	}
	if r.state.StartedCalls == nil {
		r.state.StartedCalls = map[string]NativeBlock{}
	}
	r.state.StartedCalls[call.ToolCallID] = call
	recording.failed = r.save(ctx, r.state.Phase, recording.result, map[string]any{"kind": "tool_started_outcome_unknown", "call": call})
	if recording.failed != nil {
		return recording.failed
	}
	// Cancellation persists terminal status before reading markers. A launch
	// either precedes that read with a marker, or observes terminal status here.
	return nativeBudgetCheck(ctx, r.execCtx, NativeContextPolicy{}, NativeUsage{}, 0, 0)
}

// NativeInterruptedEffects exposes checkpointed uncertainties to cancellation
// summaries and successor context without returning private provider state.
func NativeInterruptedEffects(ctx context.Context, store agentcore.Store, appID, runID string) (json.RawMessage, error) {
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
	var effects []map[string]any
	for id, call := range state.StartedCalls {
		completed := false
		for _, message := range state.Messages {
			for _, block := range message.Blocks {
				if block.Type == nativeBlockTypeToolResult && block.ToolCallID == id {
					approval, ok := nativeParseApprovalBlock(block.Output)
					if !ok || !approval.ApprovalRequired {
						completed = true
					}
				}
			}
		}
		if !completed {
			effects = append(effects, map[string]any{"tool_call_id": id, "tool_name": call.ToolName, "status": "started_outcome_unknown", "message": nativeUnknownOutcome})
		}
	}
	if len(effects) == 0 {
		return nil, nil
	}
	return json.Marshal(map[string]any{"interrupted_external_effects": effects})
}
