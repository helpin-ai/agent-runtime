package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	tclient "go.temporal.io/sdk/client"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
)

type RunEngine struct {
	client tclient.Client
}

func NewRunEngine(client tclient.Client) *RunEngine {
	if client == nil {
		return nil
	}
	return &RunEngine{client: client}
}

func (e *RunEngine) StartRun(ctx context.Context, run *agentcore.AgentRun) error {
	if e == nil || e.client == nil {
		return fmt.Errorf("temporal run engine is not configured")
	}
	if run == nil {
		return fmt.Errorf("run is required")
	}
	options := tclient.StartWorkflowOptions{
		ID:        WorkflowIDForRun(run.ID),
		TaskQueue: QueueForRuntime(run.RuntimeKind, run.InvocationMode),
	}
	_, err := e.client.ExecuteWorkflow(ctx, options, AgentRunWorkflow, AgentRunWorkflowInput{
		AppID: run.AppID,
		RunID: run.ID,
	})
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &alreadyStarted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("start temporal workflow: %w", err)
	}
	return nil
}

func (e *RunEngine) CancelRun(ctx context.Context, run *agentcore.AgentRun) error {
	if e == nil || e.client == nil || run == nil || strings.TrimSpace(run.ID) == "" {
		return nil
	}
	return e.client.CancelWorkflow(ctx, WorkflowIDForRun(run.ID), "")
}

func (e *RunEngine) ResumeRun(ctx context.Context, run *agentcore.AgentRun, payload engine.ResumePayload) error {
	if e == nil || e.client == nil || run == nil {
		return nil
	}
	workflowID := WorkflowIDForRun(run.ID)
	if strings.TrimSpace(workflowID) == "" {
		return fmt.Errorf("temporal workflow id is required")
	}
	err := e.client.SignalWorkflow(ctx, workflowID, "", WorkflowSignalResume, RunResumeSignal{
		Intent:          payload.Intent,
		Content:         payload.Content,
		ResponsePayload: payload.ResponsePayload,
		ExternalActorID: payload.ExternalActorID,
		ResumeID:        payload.ResumeID,
		InteractionID:   payload.InteractionID,
	})
	return err
}

func (e *RunEngine) InspectRun(ctx context.Context, run *agentcore.AgentRun) (string, error) {
	info, err := e.DescribeRun(ctx, run)
	if err != nil {
		return "", err
	}
	return info.State, nil
}

func (e *RunEngine) DescribeRun(ctx context.Context, run *agentcore.AgentRun) (*engine.RunExecutionInfo, error) {
	if e == nil || e.client == nil || run == nil || strings.TrimSpace(run.ID) == "" {
		return &engine.RunExecutionInfo{ExecutionMode: engine.ExecutionModeDurable, State: engine.DurableExecutionMissing}, nil
	}
	workflowID := WorkflowIDForRun(run.ID)
	description, err := e.client.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return &engine.RunExecutionInfo{ExecutionMode: engine.ExecutionModeDurable, State: engine.DurableExecutionMissing, WorkflowID: workflowID}, nil
		}
		return nil, err
	}
	if description == nil || description.WorkflowExecutionInfo == nil {
		return &engine.RunExecutionInfo{ExecutionMode: engine.ExecutionModeDurable, State: engine.DurableExecutionMissing, WorkflowID: workflowID}, nil
	}
	execution := description.WorkflowExecutionInfo
	state := engine.DurableExecutionFailed
	switch execution.Status {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		state = engine.DurableExecutionRunning
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		state = engine.DurableExecutionCompleted
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		state = engine.DurableExecutionCancelled
	}
	info := &engine.RunExecutionInfo{
		ExecutionMode: engine.ExecutionModeDurable,
		State:         state, WorkflowID: workflowID, TaskQueue: execution.TaskQueue,
		HistoryLength: execution.HistoryLength, HistorySizeBytes: execution.HistorySizeBytes,
		StateTransitionCount: execution.StateTransitionCount,
	}
	if execution.Execution != nil {
		info.TemporalRunID = execution.Execution.RunId
	}
	if execution.StartTime != nil {
		value := execution.StartTime.AsTime()
		info.StartedAt = &value
	}
	if execution.CloseTime != nil {
		value := execution.CloseTime.AsTime()
		info.ClosedAt = &value
	}
	return info, nil
}

var _ engine.DurableExecutor = (*RunEngine)(nil)
var _ engine.DurableExecutionInspector = (*RunEngine)(nil)
var _ engine.DurableExecutionDescriber = (*RunEngine)(nil)
