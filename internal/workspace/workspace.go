package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
)

const (
	ModeHostPrepared = "host_prepared"
	ModeRepository   = "repository"

	CleanupAlways     = "always"
	CleanupOnTerminal = "on_terminal"
	CleanupManual     = "manual"

	RepositoryFinalizeNone        = "none"
	RepositoryFinalizeLocalCommit = "local_commit"
	RepositoryFinalizePushBranch  = "push_branch"
	RepositoryFinalizeOpenPR      = "open_pr"
)

type Provider interface {
	PrepareWorkspace(ctx context.Context, req PrepareRequest) (*agentcore.WorkspaceLease, error)
	FinalizeWorkspace(ctx context.Context, req FinalizeRequest) (*FinalizeResult, error)
	CleanupWorkspace(ctx context.Context, req CleanupRequest) error
}

type PrepareRequest struct {
	AppID           string                 `json:"app_id"`
	RunID           string                 `json:"run_id"`
	AgentID         string                 `json:"agent_id"`
	RuntimeKind     string                 `json:"runtime_kind"`
	Target          agentcore.TargetRef    `json:"target"`
	TargetContext   *host.TargetContext    `json:"target_context,omitempty"`
	Instructions    string                 `json:"instructions,omitempty"`
	Trigger         map[string]interface{} `json:"trigger,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
	WorkspaceMode   string                 `json:"workspace_mode"`
	ExecutionConfig json.RawMessage        `json:"execution_config,omitempty"`
}

type RepositoryWorkspaceSpec struct {
	Provider       string                 `json:"provider,omitempty"`
	CloneURL       string                 `json:"clone_url"`
	Auth           *RepositoryAuth        `json:"auth,omitempty"`
	BaseBranch     string                 `json:"base_branch,omitempty"`
	WorkBranch     string                 `json:"work_branch,omitempty"`
	CommitIdentity *GitIdentity           `json:"commit_identity,omitempty"`
	FinalizePolicy string                 `json:"finalize_policy,omitempty"`
	Metadata       map[string]interface{} `json:"metadata,omitempty"`
}

type RepositoryAuth struct {
	Type        string            `json:"type,omitempty"`
	Token       string            `json:"token,omitempty"`
	Username    string            `json:"username,omitempty"`
	Password    string            `json:"password,omitempty"`
	ExtraHeader string            `json:"extra_header,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

type GitIdentity struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

type FinalizeRequest struct {
	AppID         string                   `json:"app_id"`
	RunID         string                   `json:"run_id"`
	AgentID       string                   `json:"agent_id"`
	RuntimeKind   string                   `json:"runtime_kind"`
	Target        agentcore.TargetRef      `json:"target"`
	Lease         agentcore.WorkspaceLease `json:"lease"`
	Repository    *RepositoryWorkspaceSpec `json:"repository,omitempty"`
	Outcome       string                   `json:"outcome"`
	ErrorMessage  string                   `json:"error_message,omitempty"`
	OutputSummary json.RawMessage          `json:"output_summary,omitempty"`
}

type FinalizeResult struct {
	OutputSummary json.RawMessage        `json:"output_summary,omitempty"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

type CleanupRequest struct {
	AppID       string                   `json:"app_id"`
	RunID       string                   `json:"run_id"`
	AgentID     string                   `json:"agent_id"`
	RuntimeKind string                   `json:"runtime_kind"`
	Target      agentcore.TargetRef      `json:"target"`
	Lease       agentcore.WorkspaceLease `json:"lease"`
	Repository  *RepositoryWorkspaceSpec `json:"repository,omitempty"`
	Reason      string                   `json:"reason,omitempty"`
}

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
