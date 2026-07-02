package sdk

import "encoding/json"

const (
	WorkspaceModeHostPrepared = "host_prepared"
	WorkspaceModeRepository   = "repository"

	CleanupAlways     = "always"
	CleanupOnTerminal = "on_terminal"
	CleanupManual     = "manual"

	RepositoryFinalizeNone        = "none"
	RepositoryFinalizeLocalCommit = "local_commit"
	RepositoryFinalizePushBranch  = "push_branch"
	RepositoryFinalizeOpenPR      = "open_pr"
)

type TargetContext struct {
	Target  TargetRef              `json:"target"`
	Summary string                 `json:"summary,omitempty"`
	Data    map[string]interface{} `json:"data,omitempty"`
}

type TargetContextRequest struct {
	AppID    string                 `json:"app_id"`
	RunID    string                 `json:"run_id,omitempty"`
	AgentID  string                 `json:"agent_id,omitempty"`
	Target   TargetRef              `json:"target"`
	Trigger  map[string]interface{} `json:"trigger,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type PrepareWorkspaceRequest struct {
	AppID           string                 `json:"app_id"`
	RunID           string                 `json:"run_id"`
	AgentID         string                 `json:"agent_id"`
	RuntimeKind     string                 `json:"runtime_kind"`
	Target          TargetRef              `json:"target"`
	TargetContext   *TargetContext         `json:"target_context,omitempty"`
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

type FinalizeWorkspaceRequest struct {
	AppID         string                   `json:"app_id"`
	RunID         string                   `json:"run_id"`
	AgentID       string                   `json:"agent_id"`
	RuntimeKind   string                   `json:"runtime_kind"`
	Target        TargetRef                `json:"target"`
	Lease         WorkspaceLease           `json:"lease"`
	Repository    *RepositoryWorkspaceSpec `json:"repository,omitempty"`
	Outcome       string                   `json:"outcome"`
	ErrorMessage  string                   `json:"error_message,omitempty"`
	OutputSummary json.RawMessage          `json:"output_summary,omitempty"`
}

type FinalizeWorkspaceResult struct {
	OutputSummary json.RawMessage        `json:"output_summary,omitempty"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

type CleanupWorkspaceRequest struct {
	AppID       string                   `json:"app_id"`
	RunID       string                   `json:"run_id"`
	AgentID     string                   `json:"agent_id"`
	RuntimeKind string                   `json:"runtime_kind"`
	Target      TargetRef                `json:"target"`
	Lease       WorkspaceLease           `json:"lease"`
	Repository  *RepositoryWorkspaceSpec `json:"repository,omitempty"`
	Reason      string                   `json:"reason,omitempty"`
}

type CommandExecutionContext struct {
	AppID             string                 `json:"app_id"`
	RunID             string                 `json:"run_id,omitempty"`
	AgentID           string                 `json:"agent_id,omitempty"`
	ExternalActorID   string                 `json:"external_actor_id,omitempty"`
	WorkspaceID       string                 `json:"workspace_id,omitempty"`
	TargetType        string                 `json:"target_type,omitempty"`
	TargetID          string                 `json:"target_id,omitempty"`
	Target            TargetRef              `json:"target"`
	RunInputMetadata  map[string]interface{} `json:"run_input_metadata,omitempty"`
	TargetMetadata    map[string]interface{} `json:"target_metadata,omitempty"`
	WorkspaceMetadata map[string]interface{} `json:"workspace_metadata,omitempty"`
}

type CommandExecutionRequest struct {
	Meta        CommandExecutionContext `json:"meta"`
	CommandName string                  `json:"command_name"`
	Input       json.RawMessage         `json:"input,omitempty"`
}

type CommandExecutionResponse struct {
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type Tool struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description"`
	Category             string          `json:"category,omitempty"`
	InputSchema          json.RawMessage `json:"input_schema"`
	Mutating             bool            `json:"mutating,omitempty"`
	SupportedTargetTypes []string        `json:"supported_target_types,omitempty"`
}

type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type RunToolCallRequest struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

type ProviderToolCallRequest struct {
	ToolName string                  `json:"tool_name"`
	Input    json.RawMessage         `json:"input"`
	Meta     CommandExecutionContext `json:"meta"`
}

type ToolCallResult struct {
	Content          []ContentItem `json:"content"`
	IsError          bool          `json:"is_error,omitempty"`
	ApprovalRequired bool          `json:"approval_required,omitempty"`
	InteractionID    string        `json:"interaction_id,omitempty"`
}
