package runtime

import "github.com/helpin-ai/agent-runtime/internal/workspace"

func (r *nativeRecorder) reconcileWorkspaceRecovery() {
	recovery, _ := r.execCtx.Run.Input.Metadata[workspace.RecoveryMetadataKey].(string)
	if recovery == "" || recovery == r.state.WorkspaceRecoveryID {
		return
	}
	// Never execute an already-approved command against a different tree.
	// Completed external effects and unknown-outcome markers remain in the
	// transcript; only unexecuted approval placeholders are invalidated.
	for _, pending := range nativeApprovalPlaceholders(r.state.Messages) {
		msg := &r.state.Messages[pending.MessageIndex]
		nativeSetToolResultBlock(msg, &msg.Blocks[pending.BlockIndex], "Not executed: the ephemeral workspace was lost. Inspect the fresh checkout and request a new action/approval if still needed.", true)
	}
	r.state.Messages = append(r.state.Messages, NativeMessage{Role: "user", Provenance: "host_request", Content: "The worker's ephemeral workspace was lost. A fresh repository checkout or empty analysis workspace has been prepared. Unpushed edits, local commits, installed environments, generated data, build outputs and prior local validation results are not present or valid here. For repository work, inspect the current repository/remote state and reattach any additional repositories. Recreate required local changes/data and rerun validation. The conversation records past actions, not current files. Completed external actions (including pushes, messages and API calls) may still exist; verify their state and do not blindly repeat them. Calls with unknown outcomes still require reconciliation. Pending tool approvals from the previous workspace have been invalidated."})
	r.state.WorkspaceRecoveryID = recovery
	r.state.Phase = "ready"
	r.state.Result = nil
}
