package durable

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

type AgentRunWorkflowInput struct {
	AppID              string `json:"app_id"`
	RunID              string `json:"run_id"`
	EphemeralWorkspace bool   `json:"ephemeral_workspace,omitempty"`
}

type ExecuteRunResult struct {
	WaitForApproval   bool `json:"wait_for_approval"`
	AwaitingInput     bool `json:"awaiting_input"`
	AwaitingAuth      bool `json:"awaiting_auth"`
	ContinueExecution bool `json:"continue_execution"`
}

type RunMessageSignal struct {
	Content string `json:"content"`
}

type RunResumeSignal struct {
	Intent          string          `json:"intent"`
	Content         string          `json:"content,omitempty"`
	ResponsePayload json.RawMessage `json:"response_payload,omitempty"`
	ExternalActorID string          `json:"external_actor_id,omitempty"`
	ResumeID        string          `json:"resume_id,omitempty"`
	InteractionID   string          `json:"interaction_id,omitempty"`
}

func AgentRunWorkflow(ctx workflow.Context, input AgentRunWorkflowInput) error {
	var local *localRunSession
	if input.EphemeralWorkspace {
		local = &localRunSession{ctx: ctx, input: input}
		defer local.cleanup()
	}
	// Terminal cleanup runs on the same queue/volume, including a cancellation
	// while paused. This is part of the durable workflow, not an idle reaper.
	if local == nil && workflow.GetVersion(ctx, "terminal-workspace-cleanup", workflow.DefaultVersion, 1) != workflow.DefaultVersion {
		defer func() {
			cleanupCtx, _ := workflow.NewDisconnectedContext(ctx)
			cleanupCtx = workflow.WithActivityOptions(cleanupCtx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: time.Minute, MaximumAttempts: 10}})
			if err := workflow.ExecuteActivity(cleanupCtx, "AgentRunActivities.CleanupTerminalWorkspaceActivity", input.AppID, input.RunID).Get(cleanupCtx, nil); err != nil {
				workflow.GetLogger(ctx).Error("Terminal workspace cleanup failed", "error", err)
			}
		}()
	}
	currentStage := "queued"
	waitingApproval := false
	waitingInput := false
	consumedResumeIDs := map[string]struct{}{}
	acceptResume := func(signal RunResumeSignal) bool {
		resumeID := strings.TrimSpace(signal.ResumeID)
		if resumeID == "" {
			return true
		}
		if _, duplicate := consumedResumeIDs[resumeID]; duplicate {
			return false
		}
		consumedResumeIDs[resumeID] = struct{}{}
		return true
	}

	_ = workflow.SetQueryHandler(ctx, "current_step", func() (string, error) {
		return currentStage, nil
	})
	_ = workflow.SetQueryHandler(ctx, "approval_wait_state", func() (bool, error) {
		return waitingApproval, nil
	})
	_ = workflow.SetQueryHandler(ctx, "input_wait_state", func() (bool, error) {
		return waitingInput, nil
	})

	prepareAO := workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Hour,
		// ExecuteRunActivity runs the full agent (LLM + many tool calls). It emits
		// periodic activity heartbeats; the timeout is intentionally wider than the
		// heartbeat cadence to tolerate slow provider/tool I/O and worker stalls.
		HeartbeatTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    2 * time.Minute,
			MaximumAttempts:    3,
		},
	}
	executeAO := prepareAO
	executeAO.RetryPolicy = &temporal.RetryPolicy{
		InitialInterval:    2 * time.Second,
		BackoffCoefficient: 2,
		MaximumInterval:    30 * time.Second,
		MaximumAttempts:    3,
	}
	failAO := workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    10,
		},
	}

	currentStage = "preparing"
	prepareCtx := workflow.WithActivityOptions(ctx, prepareAO)
	prepare := func() error {
		if local != nil {
			return local.prepare(prepareAO)
		}
		return workflow.ExecuteActivity(prepareCtx, "AgentRunActivities.PrepareRunActivity", input.AppID, input.RunID).Get(ctx, nil)
	}
	if err := prepare(); err != nil {
		markRunFailed(workflow.WithActivityOptions(ctx, failAO), input, err)
		return err
	}

	approveCh := workflow.GetSignalChannel(ctx, WorkflowSignalApprove)
	handoffCh := workflow.GetSignalChannel(ctx, WorkflowSignalHandoff)
	messageCh := workflow.GetSignalChannel(ctx, WorkflowSignalMessage)
	resumeCh := workflow.GetSignalChannel(ctx, WorkflowSignalResume)
	executeCtx := workflow.WithActivityOptions(ctx, executeAO)
	execute := func(result *ExecuteRunResult) error {
		if local != nil {
			return local.execute(prepareAO, executeAO, result)
		}
		return workflow.ExecuteActivity(executeCtx, "AgentRunActivities.ExecuteRunActivity", input.AppID, input.RunID).Get(ctx, result)
	}

	for {
		currentStage = "executing"
		var result ExecuteRunResult
		if err := execute(&result); err != nil {
			markRunFailed(workflow.WithActivityOptions(ctx, failAO), input, err)
			return err
		}

		if result.WaitForApproval {
			waitingApproval = true
			currentStage = "awaiting_approval"
			for waitingApproval {
				selector := workflow.NewSelector(ctx)
				selector.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
				selector.AddReceive(resumeCh, func(c workflow.ReceiveChannel, more bool) {
					var signal RunResumeSignal
					c.Receive(ctx, &signal)
					if !acceptResume(signal) {
						return
					}
					waitingApproval = false
					currentStage = workflowStageForResumeSignal(signal, true)
				})
				selector.AddReceive(approveCh, func(c workflow.ReceiveChannel, more bool) {
					var ignored struct{}
					c.Receive(ctx, &ignored)
					waitingApproval = false
					currentStage = "approval_received"
				})
				selector.AddReceive(messageCh, func(c workflow.ReceiveChannel, more bool) {
					var msg RunMessageSignal
					c.Receive(ctx, &msg)
					waitingApproval = false
					currentStage = "feedback_received"
				})
				selector.AddReceive(handoffCh, func(c workflow.ReceiveChannel, more bool) {
					var ignored any
					c.Receive(ctx, &ignored)
					currentStage = "handoff_recorded"
				})
				selector.Select(ctx)
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			continue
		}

		if result.AwaitingInput {
			waitingInput = true
			currentStage = "awaiting_input"
			for waitingInput {
				selector := workflow.NewSelector(ctx)
				selector.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
				selector.AddReceive(resumeCh, func(c workflow.ReceiveChannel, more bool) {
					var signal RunResumeSignal
					c.Receive(ctx, &signal)
					if !acceptResume(signal) {
						return
					}
					waitingInput = false
					currentStage = workflowStageForResumeSignal(signal, false)
				})
				selector.AddReceive(messageCh, func(c workflow.ReceiveChannel, more bool) {
					var msg RunMessageSignal
					c.Receive(ctx, &msg)
					waitingInput = false
					currentStage = "input_received"
				})
				selector.AddReceive(approveCh, func(c workflow.ReceiveChannel, more bool) {
					var ignored struct{}
					c.Receive(ctx, &ignored)
					waitingInput = false
					currentStage = "approval_received"
				})
				selector.AddReceive(handoffCh, func(c workflow.ReceiveChannel, more bool) {
					var ignored any
					c.Receive(ctx, &ignored)
					currentStage = "handoff_recorded"
				})
				selector.Select(ctx)
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			if currentStage == "approval_received" {
				break
			}
			continue
		}

		if result.AwaitingAuth {
			currentStage = "awaiting_auth"
			for currentStage == "awaiting_auth" {
				selector := workflow.NewSelector(ctx)
				selector.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
				selector.AddReceive(resumeCh, func(c workflow.ReceiveChannel, more bool) {
					var signal RunResumeSignal
					c.Receive(ctx, &signal)
					if !acceptResume(signal) {
						return
					}
					currentStage = workflowStageForResumeSignal(signal, false)
				})
				selector.AddReceive(handoffCh, func(c workflow.ReceiveChannel, more bool) {
					var ignored any
					c.Receive(ctx, &ignored)
					currentStage = "handoff_recorded"
				})
				selector.Select(ctx)
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			continue
		}

		if result.ContinueExecution {
			currentStage = "continuing"
			continue
		}

		break
	}

	currentStage = "completed"
	return nil
}

func markRunFailed(ctx workflow.Context, input AgentRunWorkflowInput, err error) {
	if err == nil {
		return
	}
	_ = workflow.ExecuteActivity(ctx, "AgentRunActivities.MarkRunFailedActivity", input.AppID, input.RunID, runFailureMessage(err)).Get(ctx, nil)
}

func runFailureMessage(err error) string {
	if err == nil {
		return "agent run failed"
	}
	var applicationErr *temporal.ApplicationError
	if errors.As(err, &applicationErr) {
		if applicationErr.Type() == workerInterruptedErrorType {
			return "agent run execution was interrupted after automatic recovery attempts"
		}
		return applicationErr.Message()
	}
	last := err
	for unwrapped := errors.Unwrap(last); unwrapped != nil; unwrapped = errors.Unwrap(last) {
		last = unwrapped
	}
	return last.Error()
}

func workflowStageForResumeSignal(signal RunResumeSignal, waitingApproval bool) string {
	switch signal.Intent {
	case "approve":
		return "approval_received"
	case "request_changes":
		return "feedback_received"
	case "reply":
		if waitingApproval {
			return "feedback_received"
		}
		return "input_received"
	case "auth_completed":
		return "auth_completed"
	default:
		if waitingApproval {
			return "feedback_received"
		}
		return "input_received"
	}
}
