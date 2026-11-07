package runtime

import (
	"context"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"strings"
)

// Checkpoint the instruction before acknowledging it. A crash between those
// writes replays only the acknowledgement, never another instruction message.
func applyNativeSteering(ctx context.Context, r *nativeRecorder, result *nativeExecutionResult) error {
	store, ok := r.execCtx.Store.(agentcore.SteeringStore)
	if !ok {
		return nil
	}
	commands, err := store.ListRunSteering(ctx, r.execCtx.AppID, r.execCtx.Run.ID)
	if err != nil {
		return err
	}
	changed := false
	for _, command := range commands {
		if command.State == "rejected" || command.State == "not_applied" || command.Revision <= r.state.SteeringRevision {
			continue
		}
		content := fmt.Sprintf("Job coordinator instruction (revision %d): %s", command.Revision, command.Content)
		if len(command.DeniedTools) > 0 {
			content += "\nThese tools are now prohibited: " + strings.Join(command.DeniedTools, ", ")
		}
		result.Messages = append(result.Messages, NativeMessage{Role: "user", Content: content, Provenance: "system_notification"})
		r.state.SteeringRevision = command.Revision
		changed = true
	}
	if changed {
		result.TurnFinished = false
		result.TurnOutcome = ""
		result.FinalAnswer = nil
		if err := r.save(ctx, "ready", result); err != nil {
			return err
		}
	}
	if r.state.SteeringRevision > 0 {
		return store.ApplyRunSteering(ctx, r.execCtx.AppID, r.execCtx.Run.ID, r.state.SteeringRevision)
	}
	return nil
}
func nativeSteeringAllowsTool(ctx context.Context, execCtx *ExecutionContext, name string) error {
	store, ok := execCtx.Store.(agentcore.SteeringStore)
	if !ok {
		return nil
	}
	commands, err := store.ListRunSteering(ctx, execCtx.AppID, execCtx.Run.ID)
	if err != nil {
		return fmt.Errorf("cannot verify current tool restrictions")
	}
	for _, command := range commands {
		for _, denied := range command.DeniedTools {
			if denied == name {
				return fmt.Errorf("tool %q was disabled by steering revision %d", name, command.Revision)
			}
		}
	}
	return nil
}
