package durable

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// localRunSession pins all workspace activities, including approval resumes,
// to one worker. Losing that worker creates a new session and checkout; the
// native transcript is reconciled, not discarded or blindly replayed.
type localRunSession struct {
	ctx      workflow.Context
	input    AgentRunWorkflowInput
	session  workflow.Context
	attempts int
}

func (r *localRunSession) failed(err error) bool {
	return r.ctx.Err() == nil && (errors.Is(err, workflow.ErrSessionFailed) || (r.session != nil && workflow.GetSessionInfo(r.session).SessionState == workflow.SessionStateFailed))
}

func (r *localRunSession) prepare(options workflow.ActivityOptions) error {
	for {
		if r.session != nil {
			workflow.CompleteSession(r.session)
			r.session = nil
		}
		r.attempts++
		if r.attempts > 3 {
			return temporal.NewNonRetryableApplicationError("ephemeral worker was lost repeatedly; automatic recovery exhausted", "WorkspaceRecoveryExhausted", nil)
		}
		session, err := workflow.CreateSession(r.ctx, &workflow.SessionOptions{
			CreationTimeout: 10 * time.Minute, ExecutionTimeout: 7 * 24 * time.Hour, HeartbeatTimeout: 30 * time.Second,
		})
		if err != nil {
			return err
		}
		r.session = session
		activityCtx := workflow.WithActivityOptions(session, options)
		err = workflow.ExecuteActivity(activityCtx, "AgentRunActivities.PrepareLocalRunActivity", r.input.AppID, r.input.RunID, workflow.GetSessionInfo(session).SessionID).Get(r.ctx, nil)
		if err == nil || !r.failed(err) {
			return err
		}
	}
}

func (r *localRunSession) execute(prepareOptions, executeOptions workflow.ActivityOptions, result *ExecuteRunResult) error {
	for {
		if r.session == nil || workflow.GetSessionInfo(r.session).SessionState == workflow.SessionStateFailed {
			if err := r.prepare(prepareOptions); err != nil {
				return err
			}
		}
		activityCtx := workflow.WithActivityOptions(r.session, executeOptions)
		err := workflow.ExecuteActivity(activityCtx, "AgentRunActivities.ExecuteLocalRunActivity", r.input.AppID, r.input.RunID, workflow.GetSessionInfo(r.session).SessionID).Get(r.ctx, result)
		if err == nil || !r.failed(err) {
			return err
		}
	}
}

func (r *localRunSession) cleanup() {
	if r.session == nil {
		return
	}
	defer workflow.CompleteSession(r.session)
	if workflow.GetSessionInfo(r.session).SessionState != workflow.SessionStateOpen {
		return
	}
	ctx, _ := workflow.NewDisconnectedContext(r.session)
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute, ScheduleToStartTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 2},
	})
	if err := workflow.ExecuteActivity(ctx, "AgentRunActivities.CleanupLocalWorkspaceActivity", r.input.AppID, r.input.RunID, workflow.GetSessionInfo(r.session).SessionID).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Warn("Ephemeral workspace cleanup deferred to pod deletion", "error", err)
	}
}
