package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"log/slog"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// WorkJobSubmitTool is the runtime-reserved tool through which a work-job
// worker submits item results. Only work-job workers are offered it; its
// handler takes app, run and attempt from the trusted call context.
const WorkJobSubmitTool = "submit_item_result"

// Delegation tools let a run spawn one level of subtasks through the job
// dispatcher. A run offers them only when its agent lists them; work-job
// workers never get them, so delegation stays one level deep.
var WorkJobDelegationTools = []string{"spawn_subtasks", "subtask_status", "wait_for_subtasks", "cancel_subtasks"}

type workJobRunKey struct{}

// StartWorkJobRun creates a worker run in a work-job group. It is the job
// module's entry point; hosts cannot join work-job groups through StartRun.
// req.HostRunID must be the attempt's stable dispatch key, so a retry after a
// crash returns the run that was already created. link runs after the run is
// stored and before it executes, so the worker's attempt is linked before the
// worker can submit a result; when link fails the run stays queued for the
// job reconciler.
func (e *Engine) StartWorkJobRun(ctx context.Context, req StartRunRequest, link func(*agentcore.AgentRun) error) (*agentcore.AgentRun, error) {
	if strings.TrimSpace(req.GroupID) == "" || strings.TrimSpace(req.HostRunID) == "" {
		return nil, errors.New("work job runs require a group and a dispatch key")
	}
	if _, ok := e.runGroupStore(); !ok {
		return nil, agentcore.ErrRunGroupsUnsupported
	}
	if link == nil {
		link = func(*agentcore.AgentRun) error { return nil }
	}
	return e.StartRun(context.WithValue(ctx, workJobRunKey{}, link), req)
}

func startsWorkJobRun(ctx context.Context) bool {
	_, ok := ctx.Value(workJobRunKey{}).(func(*agentcore.AgentRun) error)
	return ok
}

// linkWorkJobRun runs the job module's link hook for a new worker run and
// reports whether the run may execute now.
func linkWorkJobRun(ctx context.Context, run *agentcore.AgentRun) bool {
	link, ok := ctx.Value(workJobRunKey{}).(func(*agentcore.AgentRun) error)
	if !ok {
		return true
	}
	if err := link(run); err != nil {
		slog.WarnContext(ctx, "work job run not linked; leaving it for the job reconciler", "app_id", run.AppID, "run_id", run.ID, "error", err)
		return false
	}
	return true
}

// launchLightweight is the single admission guard for in-process execution:
// creation, resume, generic recovery and work-job recovery all start runs
// through it, so one run never executes twice at once in this process. A
// launch that arrives while the run is still executing (a resume racing the
// previous turn's wind-down) is not dropped: it runs once more after the
// current execution returns.
func (e *Engine) launchLightweight(appID, runID string) bool {
	key := appID + "\x00" + runID
	e.executingMu.Lock()
	if slot, running := e.executing[key]; running {
		slot.again = true
		e.executingMu.Unlock()
		return true
	}
	if e.executing == nil {
		e.executing = map[string]*executionSlot{}
	}
	slot := &executionSlot{}
	e.executing[key] = slot
	e.executingMu.Unlock()
	go func() {
		for {
			e.executeLightweight(context.Background(), appID, runID)
			e.executingMu.Lock()
			if slot.again {
				slot.again = false
				e.executingMu.Unlock()
				continue
			}
			delete(e.executing, key)
			e.executingMu.Unlock()
			return
		}
	}()
	return true
}

type executionSlot struct{ again bool }

// DefaultExecutionMode is the mode new runs get: durable when this engine
// has a durable executor configured as default, otherwise lightweight.
func (e *Engine) DefaultExecutionMode() string { return e.cfg.DefaultExecutionMode }

// IsExecuting reports whether this process is executing the run.
func (e *Engine) IsExecuting(appID, runID string) bool {
	e.executingMu.Lock()
	defer e.executingMu.Unlock()
	_, running := e.executing[appID+"\x00"+runID]
	return running
}

// RecoverWorkJobRun continues an interrupted work-job worker from its
// checkpoint. The job reconciler calls it after granting the run a new
// execution generation; it returns false when the run is already executing
// or cannot be recovered here.
func (e *Engine) RecoverWorkJobRun(ctx context.Context, appID, runID string) (bool, error) {
	if e.cfg.ManualLightweightExecution {
		return false, nil
	}
	run, err := e.cfg.Store.GetRun(ctx, appID, runID)
	if err != nil || run == nil {
		return false, err
	}
	if run.ExecutionMode != ExecutionModeLightweight || (run.Status != agentcore.RunStatusQueued && run.Status != agentcore.RunStatusRunning) {
		return false, nil
	}
	if e.IsExecuting(appID, runID) {
		return false, nil
	}
	e.emitRunEvent(ctx, run, "run.recovered", map[string]interface{}{"previous_status": run.Status})
	return e.launchLightweight(appID, runID), nil
}

// CancelWorkJobRuns closes a work-job group and cancels its active workers.
// Work jobs control their own group regardless of the host run-group opt-in.
func (e *Engine) CancelWorkJobRuns(ctx context.Context, appID, groupID string) error {
	groups, ok := e.runGroupStore()
	if !ok {
		return agentcore.ErrRunGroupsUnsupported
	}
	if _, err := groups.SetRunGroupState(ctx, appID, groupID, agentcore.RunGroupStateCancelled); err != nil {
		return err
	}
	members, err := groups.ListRunGroupMembers(ctx, appID, groupID)
	if err != nil {
		return err
	}
	var errs []error
	for _, member := range members {
		if agentcore.IsTerminalStatus(member.Status) {
			continue
		}
		if _, err := e.CancelRun(ctx, appID, member.RunID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// workJobWorkerTools grants the reserved submit tool to work-job workers and
// strips it from every other run, even one whose agent lists it.
func (e *Engine) workJobWorkerTools(ctx context.Context, run *agentcore.AgentRun, allowed map[string]bool) {
	worker, err := e.ownedByWorkJob(ctx, run)
	if err != nil {
		slog.WarnContext(ctx, "work job membership lookup failed", "app_id", run.AppID, "run_id", run.ID, "error", err)
	}
	if worker || err != nil {
		for _, name := range WorkJobDelegationTools {
			delete(allowed, name)
		}
	}
	if worker && err == nil {
		allowed[WorkJobSubmitTool] = true
		return
	}
	delete(allowed, WorkJobSubmitTool)
}

// WorkJobAgentPinKey holds, in a work-job worker's run metadata, the
// worker agent's system prompt and skills as they were when the job was
// created, so editing a bot during a long job does not change how later
// batches behave. Tools, approvals and credentials stay live.
const WorkJobAgentPinKey = "runtime_work_job_agent"

type workJobAgentPin struct {
	SystemPrompt string               `json:"system_prompt"`
	Skills       []agentcore.SkillRef `json:"skills,omitempty"`
}

// PinWorkJobAgent returns run metadata that pins agent's identity and the
// current versions of its workspace skills for a job's workers.
func (e *Engine) PinWorkJobAgent(ctx context.Context, agent *agentcore.Agent) (map[string]interface{}, error) {
	metadata := map[string]interface{}{WorkJobAgentPinKey: workJobAgentPin{SystemPrompt: agent.SystemPrompt, Skills: agent.Skills}}
	if len(agent.Skills) > 0 && e.cfg.Skills != nil {
		snapshot, err := e.cfg.Skills.SnapshotWorkspaceSkills(ctx, skills.LookupContext{AppID: agent.AppID, AgentID: agent.ID}, agent.Skills)
		if err != nil {
			return nil, err
		}
		if len(snapshot) > 0 {
			metadata[skills.PinnedSkillsMetadataKey] = snapshot
		}
	}
	return metadata, nil
}

// applyWorkJobAgentPin gives a work-job worker the agent identity and skill
// versions pinned when its job was created. Pins are honoured only on runs
// a work job owns; anywhere else they are dropped.
func (e *Engine) applyWorkJobAgentPin(ctx context.Context, run *agentcore.AgentRun, agent *agentcore.Agent) error {
	raw, hasPin := run.Input.Metadata[WorkJobAgentPinKey]
	_, hasSkills := run.Input.Metadata[skills.PinnedSkillsMetadataKey]
	if !hasPin && !hasSkills {
		return nil
	}
	owned, err := e.ownedByWorkJob(ctx, run)
	if err != nil {
		return err
	}
	if !owned {
		delete(run.Input.Metadata, WorkJobAgentPinKey)
		delete(run.Input.Metadata, skills.PinnedSkillsMetadataKey)
		return nil
	}
	if !hasPin {
		return nil
	}
	var pin workJobAgentPin
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, &pin); err != nil {
		return fmt.Errorf("decode work job agent pin: %w", err)
	}
	agent.SystemPrompt, agent.Skills = pin.SystemPrompt, pin.Skills
	return nil
}
