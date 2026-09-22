package durable

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
)

func TestLocalWorkflowKeepsSessionAcrossPauses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		paused ExecuteRunResult
		cancel bool
	}{
		{name: "approval", paused: ExecuteRunResult{WaitForApproval: true}},
		{name: "input", paused: ExecuteRunResult{AwaitingInput: true}},
		{name: "auth", paused: ExecuteRunResult{AwaitingAuth: true}},
		{name: "continuation", paused: ExecuteRunResult{ContinueExecution: true}},
		{name: "canceled pause", paused: ExecuteRunResult{WaitForApproval: true}, cancel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			suite := &testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			env.SetWorkerOptions(worker.Options{EnableSessionWorker: true})
			var session string
			var calls, cleaned int
			env.RegisterActivityWithOptions(func(_ context.Context, _, _, current string) error {
				if current == "" {
					t.Error("missing session")
				}
				session = current
				return nil
			}, activity.RegisterOptions{Name: "AgentRunActivities.PrepareLocalRunActivity"})
			env.RegisterActivityWithOptions(func(_ context.Context, _, _, current string) (ExecuteRunResult, error) {
				if session != current {
					t.Error("approval resumed on another session")
				}
				calls++
				if calls == 1 {
					return tt.paused, nil
				}
				return ExecuteRunResult{}, nil
			}, activity.RegisterOptions{Name: "AgentRunActivities.ExecuteLocalRunActivity"})
			env.RegisterActivityWithOptions(func(_ context.Context, _, _, current string) error {
				if session != current {
					t.Error("cleanup moved to another session")
				}
				cleaned++
				return nil
			}, activity.RegisterOptions{Name: "AgentRunActivities.CleanupLocalWorkspaceActivity"})
			env.RegisterDelayedCallback(func() {
				if tt.cancel {
					env.CancelWorkflow()
					return
				}
				env.SignalWorkflow(WorkflowSignalResume, RunResumeSignal{Intent: "reply"})
			}, time.Second)
			env.ExecuteWorkflow(AgentRunWorkflow, AgentRunWorkflowInput{AppID: "app", RunID: "run", EphemeralWorkspace: true})
			if tt.cancel {
				if env.GetWorkflowError() == nil || calls != 1 || cleaned != 1 {
					t.Fatalf("cancellation did not clean pinned session: calls=%d cleanup=%d err=%v", calls, cleaned, env.GetWorkflowError())
				}
				return
			}
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || cleaned != 1 {
				t.Fatalf("calls=%d cleaned=%d", calls, cleaned)
			}
		})
	}
}
