package durable

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

func TestAgentRunWorkflowPrepareFailureMarksRunFailed(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()

	markedAppID := ""
	markedRunID := ""
	markedError := ""

	env.RegisterWorkflow(AgentRunWorkflow)
	env.RegisterActivityWithOptions(func(ctx context.Context, appID, runID string) error {
		return errors.New("prepare exploded")
	}, activity.RegisterOptions{Name: "AgentRunActivities.PrepareRunActivity"})
	env.RegisterActivityWithOptions(func(ctx context.Context, appID, runID, errMsg string) error {
		markedAppID = appID
		markedRunID = runID
		markedError = errMsg
		return nil
	}, activity.RegisterOptions{Name: "AgentRunActivities.MarkRunFailedActivity"})

	env.ExecuteWorkflow(AgentRunWorkflow, AgentRunWorkflowInput{AppID: "app-a", RunID: "run-1"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if env.GetWorkflowError() == nil {
		t.Fatal("expected workflow error")
	}
	if markedAppID != "app-a" || markedRunID != "run-1" {
		t.Fatalf("expected failure marker for app-a/run-1, got %q/%q", markedAppID, markedRunID)
	}
	if markedError == "" {
		t.Fatal("expected failure marker to receive the workflow error")
	}
}

func TestAgentRunWorkflowRetriesFailurePersistence(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	markAttempts := 0

	env.RegisterWorkflow(AgentRunWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, string, string) error {
		return errors.New("prepare exploded")
	}, activity.RegisterOptions{Name: "AgentRunActivities.PrepareRunActivity"})
	env.RegisterActivityWithOptions(func(context.Context, string, string, string) error {
		markAttempts++
		if markAttempts < 3 {
			return errors.New("database temporarily unavailable")
		}
		return nil
	}, activity.RegisterOptions{Name: "AgentRunActivities.MarkRunFailedActivity"})

	env.ExecuteWorkflow(AgentRunWorkflow, AgentRunWorkflowInput{AppID: "app-a", RunID: "run-1"})

	if markAttempts != 3 {
		t.Fatalf("expected failure marker to retry twice, attempts=%d", markAttempts)
	}
	if env.GetWorkflowError() == nil {
		t.Fatal("expected original workflow failure")
	}
}

func TestAgentRunWorkflowIgnoresDuplicateResumeSignal(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	executeCalls := 0
	uniqueResumeSent := false
	completedBeforeUniqueResume := false

	env.RegisterWorkflow(AgentRunWorkflow)
	env.RegisterActivityWithOptions(func(context.Context, string, string) error {
		return nil
	}, activity.RegisterOptions{Name: "AgentRunActivities.PrepareRunActivity"})
	env.RegisterActivityWithOptions(func(context.Context, string, string) (ExecuteRunResult, error) {
		executeCalls++
		if executeCalls <= 2 {
			return ExecuteRunResult{AwaitingInput: true}, nil
		}
		completedBeforeUniqueResume = !uniqueResumeSent
		return ExecuteRunResult{}, nil
	}, activity.RegisterOptions{Name: "AgentRunActivities.ExecuteRunActivity"})
	env.RegisterDelayedCallback(func() {
		signal := RunResumeSignal{Intent: "reply", ResumeID: "resume-1"}
		env.SignalWorkflow(WorkflowSignalResume, signal)
		env.SignalWorkflow(WorkflowSignalResume, signal)
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		uniqueResumeSent = true
		env.SignalWorkflow(WorkflowSignalResume, RunResumeSignal{Intent: "reply", ResumeID: "resume-2"})
	}, 5*time.Second)

	env.ExecuteWorkflow(AgentRunWorkflow, AgentRunWorkflowInput{AppID: "app-a", RunID: "run-1"})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
	if executeCalls != 3 {
		t.Fatalf("expected three executions, got %d", executeCalls)
	}
	if completedBeforeUniqueResume {
		t.Fatal("duplicate resume signal advanced the next paused turn")
	}
}

func TestAgentRunWorkflowCompletesAfterExecute(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()

	executedAppID := ""
	executedRunID := ""

	env.RegisterWorkflow(AgentRunWorkflow)
	env.RegisterActivityWithOptions(func(ctx context.Context, appID, runID string) error {
		return nil
	}, activity.RegisterOptions{Name: "AgentRunActivities.PrepareRunActivity"})
	env.RegisterActivityWithOptions(func(ctx context.Context, appID, runID string) (ExecuteRunResult, error) {
		executedAppID = appID
		executedRunID = runID
		return ExecuteRunResult{}, nil
	}, activity.RegisterOptions{Name: "AgentRunActivities.ExecuteRunActivity"})

	env.ExecuteWorkflow(AgentRunWorkflow, AgentRunWorkflowInput{AppID: "app-a", RunID: "run-1"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("expected workflow success, got %v", err)
	}
	if executedAppID != "app-a" || executedRunID != "run-1" {
		t.Fatalf("expected execute for app-a/run-1, got %q/%q", executedAppID, executedRunID)
	}
}
