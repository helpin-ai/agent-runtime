package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/sdk"
)

const (
	ModeHostPrepared = sdk.WorkspaceModeHostPrepared
	ModeRepository   = sdk.WorkspaceModeRepository

	CleanupAlways     = sdk.CleanupAlways
	CleanupOnTerminal = sdk.CleanupOnTerminal
	CleanupManual     = sdk.CleanupManual

	RepositoryFinalizeNone        = sdk.RepositoryFinalizeNone
	RepositoryFinalizeLocalCommit = sdk.RepositoryFinalizeLocalCommit
	RepositoryFinalizePushBranch  = sdk.RepositoryFinalizePushBranch
	RepositoryFinalizeOpenPR      = sdk.RepositoryFinalizeOpenPR
)

type Provider interface {
	PrepareWorkspace(ctx context.Context, req PrepareRequest) (*agentcore.WorkspaceLease, error)
	FinalizeWorkspace(ctx context.Context, req FinalizeRequest) (*FinalizeResult, error)
	CleanupWorkspace(ctx context.Context, req CleanupRequest) error
}

type PrepareRequest = sdk.PrepareWorkspaceRequest
type RepositoryWorkspaceSpec = sdk.RepositoryWorkspaceSpec
type RepositoryAuth = sdk.RepositoryAuth
type GitIdentity = sdk.GitIdentity
type FinalizeRequest = sdk.FinalizeWorkspaceRequest
type FinalizeResult = sdk.FinalizeWorkspaceResult
type CleanupRequest = sdk.CleanupWorkspaceRequest

type RepositorySpecProvider interface {
	ResolveRepositoryWorkspace(ctx context.Context, req PrepareRequest) (*RepositoryWorkspaceSpec, error)
}

type Registry struct {
	providers map[string]Provider
}

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

func (r *Registry) Register(appID string, provider Provider) error {
	if r == nil {
		return fmt.Errorf("workspace registry is not configured")
	}
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return fmt.Errorf("app_id is required")
	}
	if provider == nil {
		return fmt.Errorf("workspace provider is required")
	}
	r.providers[appID] = provider
	return nil
}

func (r *Registry) Provider(appID string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	provider, ok := r.providers[strings.TrimSpace(appID)]
	return provider, ok
}

func RequiresHostPrepared(agent *agentcore.Agent) bool {
	return WorkspaceMode(agent) == ModeHostPrepared
}

func WorkspaceMode(agent *agentcore.Agent) string {
	if agent == nil || len(agent.ExecutionConfig) == 0 {
		return ""
	}
	var cfg struct {
		Workspace struct {
			Mode string `json:"mode"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(agent.ExecutionConfig, &cfg); err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Workspace.Mode)
}

func NormalizeLease(lease *agentcore.WorkspaceLease) {
	if lease == nil {
		return
	}
	lease.ID = strings.TrimSpace(lease.ID)
	lease.Provider = strings.TrimSpace(lease.Provider)
	lease.RootPath = strings.TrimSpace(lease.RootPath)
	lease.CleanupPolicy = strings.TrimSpace(lease.CleanupPolicy)
	if lease.CleanupPolicy == "" {
		lease.CleanupPolicy = CleanupOnTerminal
	}
	if lease.Metadata == nil {
		lease.Metadata = map[string]interface{}{}
	}
}

func ShouldCleanup(lease *agentcore.WorkspaceLease, terminal bool) bool {
	if lease == nil {
		return false
	}
	switch strings.TrimSpace(lease.CleanupPolicy) {
	case CleanupAlways:
		return true
	case CleanupManual:
		return false
	case "", CleanupOnTerminal:
		return terminal
	default:
		return terminal
	}
}
