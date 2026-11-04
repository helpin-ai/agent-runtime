package agentcore

import (
	"context"
	"errors"
	"strings"

	"github.com/helpin-ai/agent-runtime-go"
)

type (
	RunGroup               = sdk.RunGroup
	RunGroupPolicy         = sdk.RunGroupPolicy
	RunGroupMember         = sdk.RunGroupMember
	RunGroupCompletion     = sdk.RunGroupCompletion
	RunGroupCompletionPage = sdk.RunGroupCompletionPage
)

const (
	RunGroupOwnerHost    = sdk.RunGroupOwnerHost
	RunGroupOwnerWorkJob = sdk.RunGroupOwnerWorkJob

	RunGroupStateActive    = sdk.RunGroupStateActive
	RunGroupStatePaused    = sdk.RunGroupStatePaused
	RunGroupStateCancelled = sdk.RunGroupStateCancelled
)

var (
	ErrRunGroupsUnsupported = errors.New("run groups are not supported by this store")
	ErrRunGroupNotFound     = errors.New("run group not found")
	// ErrRunGroupClosed rejects new members of a paused or cancelled group.
	ErrRunGroupClosed = errors.New("run group is not accepting runs")
	// ErrRunGroupFull rejects a member beyond the group's MaxActiveRuns.
	ErrRunGroupFull = errors.New("run group active run limit reached")
)

// RunGroupStore is implemented by stores that persist run groups. It is kept
// separate from Store so stores without group tables are unaffected.
//
// Membership is created atomically with the run: callers attach the requested
// membership to the context passed to CreateRunWithMCP or
// CreateRunWithModelCredential. UpdateRun assigns a member's completion
// sequence in the same transaction as its first terminal status.
type RunGroupStore interface {
	RunGroupsSupported() bool
	CreateRunGroup(ctx context.Context, group *RunGroup) (*RunGroup, error)
	GetRunGroup(ctx context.Context, appID, groupID string) (*RunGroup, error)
	SetRunGroupState(ctx context.Context, appID, groupID, state string) (*RunGroup, error)
	GetRunGroupMember(ctx context.Context, appID, runID string) (*RunGroupMember, error)
	ListRunGroupMembers(ctx context.Context, appID, groupID string) ([]RunGroupMember, error)
	ListRunGroupCompletions(ctx context.Context, appID, groupID string, after int64, limit int) (*RunGroupCompletionPage, error)
	// BumpRunExecutionGeneration grants new execution authority to a member
	// and returns the new generation. Writes made under an older generation
	// must be rejected by their callers.
	BumpRunExecutionGeneration(ctx context.Context, appID, runID string) (int64, error)
}

// RunGroupMembership requests that a run being created join a group.
type RunGroupMembership struct {
	GroupID     string
	ParentRunID string
}

type runGroupMembershipKey struct{}

func WithRunGroupMembership(ctx context.Context, membership RunGroupMembership) context.Context {
	membership.GroupID = strings.TrimSpace(membership.GroupID)
	membership.ParentRunID = strings.TrimSpace(membership.ParentRunID)
	if membership.GroupID == "" {
		return ctx
	}
	return context.WithValue(ctx, runGroupMembershipKey{}, membership)
}

func RunGroupMembershipFromContext(ctx context.Context) (RunGroupMembership, bool) {
	if ctx == nil {
		return RunGroupMembership{}, false
	}
	membership, ok := ctx.Value(runGroupMembershipKey{}).(RunGroupMembership)
	return membership, ok && membership.GroupID != ""
}

const (
	DefaultRunGroupCompletionLimit = 100
	MaxRunGroupCompletionLimit     = 500
)

func NormalizeRunGroupCompletionLimit(limit int) int {
	if limit <= 0 {
		return DefaultRunGroupCompletionLimit
	}
	if limit > MaxRunGroupCompletionLimit {
		return MaxRunGroupCompletionLimit
	}
	return limit
}
