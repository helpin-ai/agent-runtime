package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// Run group unavailability reasons reported through capabilities.
const (
	RunGroupsReasonUnsupportedStore = "unsupported_store"
	RunGroupsReasonAppDisabled      = "app_disabled"
)

func (e *Engine) runGroupStore() (agentcore.RunGroupStore, bool) {
	if e == nil || e.cfg.Store == nil {
		return nil, false
	}
	groups, ok := e.cfg.Store.(agentcore.RunGroupStore)
	return groups, ok && groups.RunGroupsSupported()
}

// RunGroupsAvailable reports whether appID may use run groups, and why not.
func (e *Engine) RunGroupsAvailable(appID string) (bool, string) {
	if _, ok := e.runGroupStore(); !ok {
		return false, RunGroupsReasonUnsupportedStore
	}
	if e.cfg.RunGroupsEnabled == nil || !e.cfg.RunGroupsEnabled(strings.TrimSpace(appID)) {
		return false, RunGroupsReasonAppDisabled
	}
	return true, ""
}

func (e *Engine) requireRunGroups(appID string) (agentcore.RunGroupStore, error) {
	if ok, reason := e.RunGroupsAvailable(appID); !ok {
		if reason == RunGroupsReasonUnsupportedStore {
			return nil, agentcore.ErrRunGroupsUnsupported
		}
		return nil, ErrRunGroupsDisabled
	}
	groups, _ := e.runGroupStore()
	return groups, nil
}

// ErrRunGroupsDisabled rejects run group use by an app that has not opted in.
var ErrRunGroupsDisabled = errors.New("run groups are not enabled for this app")

// CreateRunGroup creates a host-owned group. Work-job groups are created by
// the job module, never through this host entry point.
func (e *Engine) CreateRunGroup(ctx context.Context, req agentcore.RunGroup) (*agentcore.RunGroup, error) {
	groups, err := e.requireRunGroups(req.AppID)
	if err != nil {
		return nil, err
	}
	req.ID = ""
	req.OwnerKind = agentcore.RunGroupOwnerHost
	if parent := strings.TrimSpace(req.ParentRunID); parent != "" {
		if run, err := e.cfg.Store.GetRun(ctx, req.AppID, parent); err != nil || run == nil {
			return nil, errors.New("parent run not found")
		}
	}
	return groups.CreateRunGroup(ctx, &req)
}

func (e *Engine) GetRunGroup(ctx context.Context, appID, groupID string) (*agentcore.RunGroup, error) {
	groups, err := e.requireRunGroups(appID)
	if err != nil {
		return nil, err
	}
	return groups.GetRunGroup(ctx, appID, groupID)
}

func (e *Engine) ListRunGroupMembers(ctx context.Context, appID, groupID string) ([]agentcore.RunGroupMember, error) {
	groups, err := e.requireRunGroups(appID)
	if err != nil {
		return nil, err
	}
	return groups.ListRunGroupMembers(ctx, appID, groupID)
}

func (e *Engine) ListRunGroupCompletions(ctx context.Context, appID, groupID string, after int64, limit int) (*agentcore.RunGroupCompletionPage, error) {
	groups, err := e.requireRunGroups(appID)
	if err != nil {
		return nil, err
	}
	return groups.ListRunGroupCompletions(ctx, appID, groupID, after, limit)
}

// CancelRunGroup closes admission permanently, then cancels every active
// member. Accepted results and terminal members are untouched. Member
// cancellation errors are returned after all members were attempted;
// repeating the call retries the remaining members.
func (e *Engine) CancelRunGroup(ctx context.Context, appID, groupID string) (*agentcore.RunGroup, error) {
	groups, err := e.requireRunGroups(appID)
	if err != nil {
		return nil, err
	}
	group, err := groups.SetRunGroupState(ctx, appID, groupID, agentcore.RunGroupStateCancelled)
	if err != nil {
		return nil, err
	}
	members, err := groups.ListRunGroupMembers(ctx, appID, groupID)
	if err != nil {
		return nil, err
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
	return group, errors.Join(errs...)
}

// PauseRunGroup closes admission and pauses members that support manual
// pause (durable runs). Other members continue to their next boundary.
func (e *Engine) PauseRunGroup(ctx context.Context, appID, groupID string) (*agentcore.RunGroup, error) {
	groups, err := e.requireRunGroups(appID)
	if err != nil {
		return nil, err
	}
	group, err := groups.SetRunGroupState(ctx, appID, groupID, agentcore.RunGroupStatePaused)
	if err != nil {
		return nil, err
	}
	members, err := groups.ListRunGroupMembers(ctx, appID, groupID)
	if err != nil {
		return nil, err
	}
	for _, member := range members {
		if member.Status != agentcore.RunStatusQueued && member.Status != agentcore.RunStatusRunning {
			continue
		}
		run, err := e.cfg.Store.GetRun(ctx, appID, member.RunID)
		if err != nil || run == nil || run.ExecutionMode != ExecutionModeDurable {
			continue
		}
		if _, err := e.PauseRun(ctx, appID, member.RunID); err != nil {
			slog.WarnContext(ctx, "run group member pause failed", "app_id", appID, "group_id", groupID, "run_id", member.RunID, "error", err)
		}
	}
	return group, nil
}

// ResumeRunGroup reopens admission and continues manually paused members.
func (e *Engine) ResumeRunGroup(ctx context.Context, appID, groupID string) (*agentcore.RunGroup, error) {
	groups, err := e.requireRunGroups(appID)
	if err != nil {
		return nil, err
	}
	group, err := groups.SetRunGroupState(ctx, appID, groupID, agentcore.RunGroupStateActive)
	if err != nil {
		return nil, err
	}
	members, err := groups.ListRunGroupMembers(ctx, appID, groupID)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, member := range members {
		if member.Status != agentcore.RunStatusPaused {
			continue
		}
		run, err := e.cfg.Store.GetRun(ctx, appID, member.RunID)
		if err != nil || run == nil || run.PauseReason != agentcore.PauseReasonManual {
			continue
		}
		if _, err := e.ResumeRun(ctx, appID, member.RunID, ResumePayload{Intent: agentcore.ResumeIntentContinue}); err != nil {
			errs = append(errs, err)
		}
	}
	return group, errors.Join(errs...)
}

// ownedByWorkJob reports whether a run is a worker in a work-job group. Only
// the job reconciler recovers such runs. A membership lookup error is
// reported so callers skip the run rather than risk executing it twice.
func (e *Engine) ownedByWorkJob(ctx context.Context, run *agentcore.AgentRun) (bool, error) {
	groups, ok := e.runGroupStore()
	if !ok || run == nil {
		return false, nil
	}
	member, err := groups.GetRunGroupMember(ctx, run.AppID, run.ID)
	if err != nil || member == nil {
		return false, err
	}
	group, err := groups.GetRunGroup(ctx, run.AppID, member.GroupID)
	if err != nil {
		return false, err
	}
	return group.OwnerKind == agentcore.RunGroupOwnerWorkJob, nil
}
