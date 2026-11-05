package workjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	sdk "github.com/helpin-ai/agent-runtime-go"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/mcp"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// Model-directed delegation (phase 3). A run whose agent lists these tools
// can split work it discovers into subtasks: each spawn creates a work job
// whose workers are runs of the same agent, with the parent's target,
// model, credentials and MCP servers, and never more permissions. Waiting
// does not take a job worker slot, so a parent cannot block its own
// children. Workers never get these tools: delegation is one level deep.

const (
	maxSubtaskWait     = 10 * time.Minute
	defaultSubtaskWait = 2 * time.Minute
	maxReturnedResults = 50
	subtaskPoll        = 500 * time.Millisecond
)

func registerDelegationTools(registry *tools.Registry, s *Service) {
	object := func(properties map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	jobID := map[string]any{"type": "string", "description": "The subtask job id returned by spawn_subtasks."}
	registry.Register(tools.Definition{
		Name:        "spawn_subtasks",
		Description: "Split work into subtasks handled by fresh copies of you, ten items at a time, while you continue. Use it for many similar items you discovered (pages, records, sub-questions). Each subtask returns a result matching result_schema. Then call wait_for_subtasks.",
		Category:    "Delegation", Mutating: false, RiskLevel: tools.RiskLevelRead,
		InputSchema: object(map[string]any{
			"objective":     map[string]any{"type": "string"},
			"instructions":  map[string]any{"type": "string", "description": "How to process one item."},
			"result_schema": map[string]any{"type": "object", "description": "JSON Schema of one item's result."},
			"items": map[string]any{"type": "array", "minItems": 1, "maxItems": 500, "items": object(map[string]any{
				"id": map[string]any{"type": "string"}, "input": map[string]any{"type": "object"},
			}, "id")},
			"batch_size":      map[string]any{"type": "integer", "minimum": 1, "maximum": 50},
			"max_concurrency": map[string]any{"type": "integer", "minimum": 1, "maximum": 4},
		}, "objective", "instructions", "result_schema", "items"),
	}, s.handleSpawn)
	registry.Register(tools.Definition{
		Name: "subtask_status", Description: "Progress of the subtasks you spawned. Omit job_id for all of them.",
		Category: "Delegation", RiskLevel: tools.RiskLevelRead,
		InputSchema: object(map[string]any{"job_id": jobID}),
	}, s.handleSubtaskStatus)
	registry.Register(tools.Definition{
		Name: "wait_for_subtasks", Description: "Wait until a subtask job finishes or needs a decision, up to timeout_seconds (default 120, at most 600), then return its progress and results. Call again to keep waiting.",
		Category: "Delegation", RiskLevel: tools.RiskLevelRead,
		InputSchema: object(map[string]any{"job_id": jobID, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 600}}, "job_id"),
	}, s.handleWaitSubtasks)
	registry.Register(tools.Definition{
		Name: "cancel_subtasks", Description: "Cancel a subtask job. Finished results are kept.",
		Category: "Delegation", RiskLevel: tools.RiskLevelRead,
		InputSchema: object(map[string]any{"job_id": jobID}, "job_id"),
	}, s.handleCancelSubtasks)
}

type spawnInput struct {
	Objective      string                    `json:"objective"`
	Instructions   string                    `json:"instructions"`
	ResultSchema   json.RawMessage           `json:"result_schema"`
	Items          []agentcore.WorkItemInput `json:"items"`
	BatchSize      int                       `json:"batch_size"`
	MaxConcurrency int                       `json:"max_concurrency"`
}

// handleSpawn creates a subtask job for the calling run. Everything about
// the workers comes from the parent run itself; the model chooses only the
// work.
func (s *Service) handleSpawn(ctx context.Context, call tools.CallContext, raw json.RawMessage) (json.RawMessage, error) {
	if err := s.require(call.AppID); err != nil {
		return nil, errors.New("subtasks are not available for this app")
	}
	parent, err := s.store.GetRun(ctx, call.AppID, call.RunID)
	if err != nil || parent == nil {
		return nil, errors.New("calling run not found")
	}
	if member, err := s.store.GetRunGroupMember(ctx, call.AppID, call.RunID); err != nil || (member != nil && member.Hidden) {
		return nil, errors.New("subtasks cannot spawn further subtasks")
	}
	var input spawnInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("invalid spawn_subtasks input: %v", err)
	}
	worker := agentcore.WorkJobWorker{
		AgentID: parent.AgentID, Target: parent.Target, Model: parent.Input.Model, ExternalActorID: parent.ExternalActorID,
		Metadata: map[string]interface{}{"parent_run_id": parent.ID},
	}
	for _, tool := range parent.Input.AllowedTools {
		if !slices.Contains(engine.WorkJobDelegationTools, tool) {
			worker.AllowedTools = append(worker.AllowedTools, tool)
		}
	}
	if s.cfg.Credentials != nil {
		credential, provider, ok, err := s.cfg.Credentials.CredentialForRun(ctx, parent)
		if err != nil {
			return nil, fmt.Errorf("cannot pass your model credential to subtasks: %v", err)
		}
		if ok {
			worker.ModelCredential = &credential
			if worker.Model == nil {
				worker.Model = &sdk.RunModel{Provider: provider}
			}
		}
	}
	servers, err := mcp.InheritedServers(ctx, s.store, call.AppID, call.RunID, s.cfg.RunMCP)
	if err != nil {
		return nil, fmt.Errorf("cannot pass your tools to subtasks: %v", err)
	}
	if len(servers) > 0 {
		encoded, _ := json.Marshal(servers)
		_ = json.Unmarshal(encoded, &worker.MCPServers)
	}
	digest := sha256.Sum256(append([]byte(call.RunID+"\x00"), raw...))
	job, err := s.Create(ctx, agentcore.CreateWorkJobRequest{
		AppID: call.AppID, IdempotencyKey: "subtasks:" + hex.EncodeToString(digest[:16]), ParentRunID: parent.ID, Worker: worker, Items: input.Items,
		Spec: agentcore.WorkJobSpec{Objective: input.Objective, Instructions: input.Instructions, ResultSchema: input.ResultSchema,
			BatchSize: input.BatchSize, MaxConcurrency: min(max(input.MaxConcurrency, 1), 4)},
	})
	var validation *ValidationError
	if errors.As(err, &validation) {
		return nil, errors.New(validation.Message)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"job_id": job.ID, "items": job.Counts.Total, "state": job.State,
		"next": "Call wait_for_subtasks with this job_id to collect the results."})
}

// subtaskJob returns a job only to the run that spawned it.
func (s *Service) subtaskJob(ctx context.Context, call tools.CallContext, jobID string) (*agentcore.WorkJob, error) {
	job, err := s.store.GetWorkJob(ctx, call.AppID, strings.TrimSpace(jobID))
	if err != nil || job.ParentRunID != call.RunID {
		return nil, errors.New("subtask job not found")
	}
	return job, nil
}

func subtaskSummary(job *agentcore.WorkJob) map[string]any {
	summary := map[string]any{"job_id": job.ID, "state": job.State, "counts": job.Counts}
	if job.WaitingReason != "" {
		summary["waiting_reason"] = job.WaitingReason
	}
	if job.Error != "" {
		summary["error"] = job.Error
	}
	return summary
}

func (s *Service) handleSubtaskStatus(ctx context.Context, call tools.CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var input struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(raw, &input)
	if input.JobID != "" {
		job, err := s.subtaskJob(ctx, call, input.JobID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(subtaskSummary(job))
	}
	jobs, err := s.store.ListWorkJobs(ctx, call.AppID)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for i := range jobs {
		if jobs[i].ParentRunID == call.RunID {
			out = append(out, subtaskSummary(&jobs[i]))
		}
	}
	return json.Marshal(map[string]any{"jobs": out})
}

// handleWaitSubtasks waits without holding a job worker slot: the parent is
// not a job worker, so its children keep running while it waits.
func (s *Service) handleWaitSubtasks(ctx context.Context, call tools.CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var input struct {
		JobID          string `json:"job_id"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("invalid wait_for_subtasks input: %v", err)
	}
	wait := defaultSubtaskWait
	if input.TimeoutSeconds > 0 {
		wait = min(time.Duration(input.TimeoutSeconds)*time.Second, maxSubtaskWait)
	}
	deadline := time.Now().Add(wait)
	for {
		job, err := s.subtaskJob(ctx, call, input.JobID)
		if err != nil {
			return nil, err
		}
		settled := agentcore.IsTerminalWorkJobState(job.State) || (job.State == agentcore.WorkJobStateWaiting && userDecision(job.WaitingReason))
		if settled || !time.Now().Before(deadline) {
			summary := subtaskSummary(job)
			summary["finished"] = settled
			if settled {
				dataset, err := s.store.GetWorkJobDataset(ctx, call.AppID, job.ID)
				if err == nil && dataset != nil {
					results := dataset.Items
					if len(results) > maxReturnedResults {
						summary["results_truncated"] = fmt.Sprintf("showing %d of %d results", maxReturnedResults, len(results))
						results = results[:maxReturnedResults]
					}
					summary["results"] = results
				}
			} else {
				summary["next"] = "Not finished yet; call wait_for_subtasks again."
			}
			return json.Marshal(summary)
		}
		s.Wake()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(subtaskPoll):
		}
	}
}

func (s *Service) handleCancelSubtasks(ctx context.Context, call tools.CallContext, raw json.RawMessage) (json.RawMessage, error) {
	var input struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(raw, &input)
	job, err := s.subtaskJob(ctx, call, input.JobID)
	if err != nil {
		return nil, err
	}
	if job, err = s.Cancel(ctx, call.AppID, job.ID); err != nil {
		return nil, err
	}
	return json.Marshal(subtaskSummary(job))
}

// userDecision reports waiting reasons only a person can clear.
func userDecision(reason string) bool {
	switch reason {
	case agentcore.WorkJobWaitingNoProgress, agentcore.WorkJobWaitingExceptions, agentcore.WorkJobWaitingBudget,
		agentcore.WorkJobWaitingWorkerAuth, agentcore.WorkJobWaitingUncertainEffects:
		return true
	}
	return false
}
