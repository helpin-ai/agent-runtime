package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/id"
)

// The memory store keeps run groups exactly as durable as its runs, so engine
// tests exercise the same group contract as the SQL store.

type memoryRunGroupMember struct {
	appID          string
	member         agentcore.RunGroupMember
	terminalStatus string
	completedAt    time.Time
	createdAt      time.Time
}

func (m *Memory) RunGroupsSupported() bool { return m != nil }

func (m *Memory) CreateRunGroup(_ context.Context, group *agentcore.RunGroup) (*agentcore.RunGroup, error) {
	if group == nil || strings.TrimSpace(group.AppID) == "" {
		return nil, fmt.Errorf("run group app_id is required")
	}
	cp := *group
	cp.AppID = strings.TrimSpace(cp.AppID)
	cp.ID = strings.TrimSpace(cp.ID)
	cp.CreationKey = strings.TrimSpace(cp.CreationKey)
	cp.ParentRunID = strings.TrimSpace(cp.ParentRunID)
	if cp.OwnerKind == "" {
		cp.OwnerKind = agentcore.RunGroupOwnerHost
	}
	if cp.OwnerKind != agentcore.RunGroupOwnerHost && cp.OwnerKind != agentcore.RunGroupOwnerWorkJob {
		return nil, fmt.Errorf("unsupported run group owner %q", cp.OwnerKind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cp.CreationKey != "" {
		if existingID, ok := m.runGroupKeys[key(cp.AppID, cp.CreationKey)]; ok {
			existing := *m.runGroups[key(cp.AppID, existingID)]
			return &existing, nil
		}
	}
	if cp.ID == "" {
		cp.ID = id.New("rungroup")
	}
	if _, exists := m.runGroups[key(cp.AppID, cp.ID)]; exists {
		return nil, fmt.Errorf("run group already exists")
	}
	now := time.Now().UTC()
	cp.State = agentcore.RunGroupStateActive
	cp.LastCompletionSeq = 0
	cp.CreatedAt, cp.UpdatedAt = now, now
	m.runGroups[key(cp.AppID, cp.ID)] = &cp
	if cp.CreationKey != "" {
		m.runGroupKeys[key(cp.AppID, cp.CreationKey)] = cp.ID
	}
	out := cp
	return &out, nil
}

func (m *Memory) GetRunGroup(_ context.Context, appID, groupID string) (*agentcore.RunGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	group := m.runGroups[key(strings.TrimSpace(appID), strings.TrimSpace(groupID))]
	if group == nil {
		return nil, agentcore.ErrRunGroupNotFound
	}
	out := *group
	return &out, nil
}

func (m *Memory) SetRunGroupState(_ context.Context, appID, groupID, state string) (*agentcore.RunGroup, error) {
	if err := validRunGroupState(state); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	group := m.runGroups[key(strings.TrimSpace(appID), strings.TrimSpace(groupID))]
	if group == nil {
		return nil, agentcore.ErrRunGroupNotFound
	}
	if group.State == agentcore.RunGroupStateCancelled && state != agentcore.RunGroupStateCancelled {
		return nil, agentcore.ErrRunGroupClosed
	}
	if group.State != state {
		group.State = state
		group.UpdatedAt = time.Now().UTC()
	}
	out := *group
	return &out, nil
}

func (m *Memory) GetRunGroupMember(_ context.Context, appID, runID string) (*agentcore.RunGroupMember, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k := key(strings.TrimSpace(appID), strings.TrimSpace(runID))
	stored := m.runGroupMembers[k]
	if stored == nil {
		return nil, nil
	}
	member := stored.member
	if run := m.runs[k]; run != nil {
		member.Status = run.Status
	}
	return &member, nil
}

func (m *Memory) ListRunGroupMembers(_ context.Context, appID, groupID string) ([]agentcore.RunGroupMember, error) {
	appID, groupID = strings.TrimSpace(appID), strings.TrimSpace(groupID)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.runGroups[key(appID, groupID)] == nil {
		return nil, agentcore.ErrRunGroupNotFound
	}
	stored := m.groupMembersLocked(appID, groupID)
	sort.Slice(stored, func(i, j int) bool {
		if stored[i].createdAt.Equal(stored[j].createdAt) {
			return stored[i].member.RunID < stored[j].member.RunID
		}
		return stored[i].createdAt.Before(stored[j].createdAt)
	})
	out := make([]agentcore.RunGroupMember, 0, len(stored))
	for _, item := range stored {
		member := item.member
		if run := m.runs[key(appID, member.RunID)]; run != nil {
			member.Status = run.Status
		}
		out = append(out, member)
	}
	return out, nil
}

func (m *Memory) ListRunGroupCompletions(_ context.Context, appID, groupID string, after int64, limit int) (*agentcore.RunGroupCompletionPage, error) {
	appID, groupID = strings.TrimSpace(appID), strings.TrimSpace(groupID)
	if after < 0 {
		after = 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.runGroups[key(appID, groupID)] == nil {
		return nil, agentcore.ErrRunGroupNotFound
	}
	completed := make([]*memoryRunGroupMember, 0)
	for _, item := range m.groupMembersLocked(appID, groupID) {
		if item.member.CompletionSeq > after {
			completed = append(completed, item)
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].member.CompletionSeq < completed[j].member.CompletionSeq })
	if limit = agentcore.NormalizeRunGroupCompletionLimit(limit); len(completed) > limit {
		completed = completed[:limit]
	}
	page := &agentcore.RunGroupCompletionPage{Items: make([]agentcore.RunGroupCompletion, 0, len(completed)), NextAfter: after}
	for _, item := range completed {
		page.Items = append(page.Items, agentcore.RunGroupCompletion{Seq: item.member.CompletionSeq, RunID: item.member.RunID, Status: item.terminalStatus, CompletedAt: item.completedAt})
		page.NextAfter = item.member.CompletionSeq
	}
	return page, nil
}

func (m *Memory) BumpRunExecutionGeneration(_ context.Context, appID, runID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.runGroupMembers[key(strings.TrimSpace(appID), strings.TrimSpace(runID))]
	if stored == nil {
		return 0, fmt.Errorf("run is not a run group member")
	}
	stored.member.ExecutionGeneration++
	return stored.member.ExecutionGeneration, nil
}

// admitRunGroupMemberLocked mirrors SQL admission. Callers hold m.mu and
// insert the run only if this returns nil.
func (m *Memory) admitRunGroupMemberLocked(ctx context.Context, run *agentcore.AgentRun) (*memoryRunGroupMember, error) {
	membership, ok := agentcore.RunGroupMembershipFromContext(ctx)
	if !ok {
		return nil, nil
	}
	group := m.runGroups[key(run.AppID, membership.GroupID)]
	if group == nil {
		return nil, agentcore.ErrRunGroupNotFound
	}
	if group.State != agentcore.RunGroupStateActive {
		return nil, agentcore.ErrRunGroupClosed
	}
	if max := group.Policy.MaxActiveRuns; max > 0 {
		active := 0
		for _, item := range m.groupMembersLocked(group.AppID, group.ID) {
			if run := m.runs[key(group.AppID, item.member.RunID)]; run != nil && !agentcore.IsTerminalStatus(run.Status) {
				active++
			}
		}
		if active >= max {
			return nil, agentcore.ErrRunGroupFull
		}
	}
	return &memoryRunGroupMember{
		appID: run.AppID,
		member: agentcore.RunGroupMember{
			RunID:       run.ID,
			GroupID:     group.ID,
			ParentRunID: membership.ParentRunID,
			Hidden:      group.OwnerKind != agentcore.RunGroupOwnerHost,
		},
		createdAt: time.Now().UTC(),
	}, nil
}

// assignRunGroupCompletionLocked records a member's first terminal status.
// Callers hold m.mu, so the run update and the sequence are atomic.
func (m *Memory) assignRunGroupCompletionLocked(run *agentcore.AgentRun) {
	if !agentcore.IsTerminalStatus(run.Status) {
		return
	}
	stored := m.runGroupMembers[key(run.AppID, run.ID)]
	if stored == nil || stored.member.CompletionSeq > 0 {
		return
	}
	group := m.runGroups[key(run.AppID, stored.member.GroupID)]
	if group == nil {
		return
	}
	group.LastCompletionSeq++
	stored.member.CompletionSeq = group.LastCompletionSeq
	stored.terminalStatus = run.Status
	stored.completedAt = time.Now().UTC()
	if run.CompletedAt != nil {
		stored.completedAt = run.CompletedAt.UTC()
	}
}

func (m *Memory) hiddenRunGroupMemberLocked(appID, runID string) bool {
	stored := m.runGroupMembers[key(appID, runID)]
	return stored != nil && stored.member.Hidden
}

func (m *Memory) groupMembersLocked(appID, groupID string) []*memoryRunGroupMember {
	out := make([]*memoryRunGroupMember, 0)
	for _, item := range m.runGroupMembers {
		if item.appID == appID && item.member.GroupID == groupID {
			out = append(out, item)
		}
	}
	return out
}
