package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	})
	if shouldIgnoreMissingWorkflow(err) {
		return nil
	}
	return err
}

func shouldIgnoreMissingWorkflow(err error) bool {
	if err == nil {
		return false
	}
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}

var _ engine.DurableExecutor = (*RunEngine)(nil)
