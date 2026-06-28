package agentcore

import (
	"encoding/json"
	"time"
)

const (
	RuntimeNativeSDK = "native_sdk"
	RuntimeCodex     = "codex"
	RuntimeOpenCode  = "opencode"

	InvocationAutonomous  = "autonomous"
	InvocationInteractive = "interactive"

	ApprovalModeNever  = "never"
	ApprovalModeAlways = "always"

	RunStatusQueued    = "queued"
	RunStatusRunning   = "running"
	RunStatusPaused    = "paused"
	RunStatusCompleted = "completed"
	RunStatusFailed    = "failed"
	RunStatusCancelled = "cancelled"

	PauseReasonNone          = "none"
	PauseReasonHumanInput    = "human_input"
	PauseReasonHumanApproval = "human_approval"
	PauseReasonAuth          = "authentication"
	PauseReasonUserMessage   = "awaiting_user_message"

	TurnPolicyCompleteOnFinish = "complete_on_finish"
	TurnPolicyPauseAfterAssist = "pause_after_assistant"

	ApprovalNotRequired = "not_required"
	ApprovalPending     = "pending"
	ApprovalApproved    = "approved"
	ApprovalRejected    = "rejected"
)

type TargetRef struct {
	Type     string                 `json:"type"`
	ID       string                 `json:"id"`
	Display  *TargetDisplay         `json:"display,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type TargetDisplay struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

type Agent struct {
	ID                    string          `json:"id"`
	AppID                 string          `json:"app_id"`
	Name                  string          `json:"name"`
	RuntimeKind           string          `json:"runtime_kind"`
	Provider              string          `json:"provider,omitempty"`
	Model                 string          `json:"model,omitempty"`
	SystemPrompt          string          `json:"system_prompt,omitempty"`
	Skills                []SkillRef      `json:"skills,omitempty"`
	AllowedTools          []string        `json:"allowed_tools,omitempty"`
	AllowedTargets        []string        `json:"allowed_targets,omitempty"`
	ApprovalMode          string          `json:"approval_mode"`
	DefaultInvocationMode string          `json:"default_invocation_mode"`
	ExecutionConfig       json.RawMessage `json:"execution_config,omitempty"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
}

type SkillRef struct {
	SkillID    string          `json:"skill_id,omitempty"`
	Key        string          `json:"key,omitempty"`
	Version    string          `json:"version,omitempty"`
	VersionKey string          `json:"version_key,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
}

type AgentRun struct {
	ID              string          `json:"id"`
	AppID           string          `json:"app_id"`
	AgentID         string          `json:"agent_id"`
	Target          TargetRef       `json:"target"`
	RuntimeKind     string          `json:"runtime_kind"`
	ExecutionMode   string          `json:"execution_mode"`
	InvocationMode  string          `json:"invocation_mode"`
	ExternalActorID string          `json:"external_actor_id,omitempty"`
	Status          string          `json:"status"`
	PauseReason     string          `json:"pause_reason"`
	ApprovalState   string          `json:"approval_state"`
	Input           RunInput        `json:"input"`
	OutputSummary   json.RawMessage `json:"output_summary,omitempty"`
	WorkspaceLease  *WorkspaceLease `json:"workspace_lease,omitempty"`
	ErrorMessage    string          `json:"error_message,omitempty"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	CompletedAt     *time.Time      `json:"completed_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type RunInput struct {
	Instructions   string                 `json:"instructions,omitempty"`
	AllowedTools   []string               `json:"allowed_tools,omitempty"`
	Trigger        map[string]interface{} `json:"trigger,omitempty"`
	Metadata       map[string]interface{} `json:"metadata,omitempty"`
	ContextSummary string                 `json:"context_summary,omitempty"`
	TurnPolicy     TurnPolicy             `json:"turn_policy,omitempty"`
}

type TurnPolicy struct {
	Mode                  string `json:"mode,omitempty"`
	IdleTimeoutSeconds    int    `json:"idle_timeout_seconds,omitempty"`
	ExpiredResumeStrategy string `json:"expired_resume_strategy,omitempty"`
}

type AgentRunMessage struct {
	ID              string          `json:"id"`
	AppID           string          `json:"app_id"`
	RunID           string          `json:"run_id"`
	Role            string          `json:"role"`
	Content         string          `json:"content"`
	MessageType     string          `json:"message_type"`
	ContentBlocks   json.RawMessage `json:"content_blocks,omitempty"`
	ToolInvocations json.RawMessage `json:"tool_invocations,omitempty"`
	SequenceNo      int             `json:"sequence_no"`
	CreatedAt       time.Time       `json:"created_at"`
}

type AgentRunArtifact struct {
	ID            string          `json:"id"`
	AppID         string          `json:"app_id"`
	RunID         string          `json:"run_id"`
	ArtifactType  string          `json:"artifact_type"`
	Format        string          `json:"format"`
	StorageMode   string          `json:"storage_mode"`
	InlineContent string          `json:"inline_content,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	SequenceNo    int             `json:"sequence_no"`
	CreatedAt     time.Time       `json:"created_at"`
}

type AgentRunInteraction struct {
	ID                   string          `json:"id"`
	AppID                string          `json:"app_id"`
	RunID                string          `json:"run_id"`
	RuntimeKind          string          `json:"runtime_kind"`
	InteractionKind      string          `json:"interaction_kind"`
	Status               string          `json:"status"`
	Title                string          `json:"title,omitempty"`
	Summary              string          `json:"summary,omitempty"`
	RequestPayload       json.RawMessage `json:"request_payload,omitempty"`
	ResponsePayload      json.RawMessage `json:"response_payload,omitempty"`
	ResolvedByExternalID string          `json:"resolved_by_external_id,omitempty"`
	ResolvedAt           *time.Time      `json:"resolved_at,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

type ToolCall struct {
	ID               string          `json:"id"`
	AppID            string          `json:"app_id"`
	RunID            string          `json:"run_id"`
	ToolName         string          `json:"tool_name"`
	Input            json.RawMessage `json:"input"`
	Output           json.RawMessage `json:"output,omitempty"`
	Error            string          `json:"error,omitempty"`
	Mutating         bool            `json:"mutating"`
	ApprovalRequired bool            `json:"approval_required"`
	CreatedAt        time.Time       `json:"created_at"`
}

type WorkspaceLease struct {
	ID            string                 `json:"id"`
	Provider      string                 `json:"provider,omitempty"`
	RootPath      string                 `json:"root_path"`
	CleanupPolicy string                 `json:"cleanup_policy,omitempty"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}
