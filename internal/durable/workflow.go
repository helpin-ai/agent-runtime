package durable

import (
	"encoding/json"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

type AgentRunWorkflowInput struct {
	AppID string `json:"app_id"`
	RunID string `json:"run_id"`
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
}

func AgentRunWorkflow(ctx workflow.Context, input AgentRunWorkflowInput) error {
	currentStage := "queued"
	waitingApproval := false
	waitingInput := false

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
		HeartbeatTimeout:    60 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    2 * time.Minute,
			MaximumAttempts:    3,
		},
	}
	executeAO := prepareAO
	executeAO.RetryPolicy = &temporal.RetryPolicy{MaximumAttempts: 1}
	failAO := workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}

	currentStage = "preparing"
	prepareCtx := workflow.WithActivityOptions(ctx, prepareAO)
	if err := workflow.ExecuteActivity(prepareCtx, "AgentRunActivities.PrepareRunActivity", input.AppID, input.RunID).Get(ctx, nil); err != nil {
		markRunFailed(workflow.WithActivityOptions(ctx, failAO), input, err)
		return err
	}

	approveCh := workflow.GetSignalChannel(ctx, WorkflowSignalApprove)
	handoffCh := workflow.GetSignalChannel(ctx, WorkflowSignalHandoff)
	messageCh := workflow.GetSignalChannel(ctx, WorkflowSignalMessage)
	resumeCh := workflow.GetSignalChannel(ctx, WorkflowSignalResume)
	executeCtx := workflow.WithActivityOptions(ctx, executeAO)

	for {
		currentStage = "executing"
		var result ExecuteRunResult
		if err := workflow.ExecuteActivity(executeCtx, "AgentRunActivities.ExecuteRunActivity", input.AppID, input.RunID).Get(ctx, &result); err != nil {
			markRunFailed(workflow.WithActivityOptions(ctx, failAO), input, err)
			return err
		}

		if result.WaitForApproval {
			waitingApproval = true
			currentStage = "awaiting_approval"
			for waitingApproval {
				selector := workflow.NewSelector(ctx)
				selector.AddReceive(resumeCh, func(c workflow.ReceiveChannel, more bool) {
					var signal RunResumeSignal
					c.Receive(ctx, &signal)
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
			}
			continue
		}

		if result.AwaitingInput {
			waitingInput = true
			currentStage = "awaiting_input"
			for waitingInput {
				selector := workflow.NewSelector(ctx)
				selector.AddReceive(resumeCh, func(c workflow.ReceiveChannel, more bool) {
					var signal RunResumeSignal
					c.Receive(ctx, &signal)
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
				selector.AddReceive(resumeCh, func(c workflow.ReceiveChannel, more bool) {
					var signal RunResumeSignal
					c.Receive(ctx, &signal)
					currentStage = workflowStageForResumeSignal(signal, false)
				})
				selector.AddReceive(handoffCh, func(c workflow.ReceiveChannel, more bool) {
					var ignored any
					c.Receive(ctx, &ignored)
					currentStage = "handoff_recorded"
				})
				selector.Select(ctx)
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
	_ = workflow.ExecuteActivity(ctx, "AgentRunActivities.MarkRunFailedActivity", input.AppID, input.RunID, err.Error()).Get(ctx, nil)
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
