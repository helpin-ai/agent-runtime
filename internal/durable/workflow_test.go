package durable

import (
	"context"
	"errors"
	"testing"

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
