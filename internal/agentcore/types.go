package agentcore

import "github.com/helpin-ai/agent-runtime-go"

const (
	RuntimeNativeSDK = sdk.RuntimeNativeSDK
	RuntimeCodex     = sdk.RuntimeCodex
	RuntimeOpenCode  = sdk.RuntimeOpenCode

	InvocationAutonomous  = sdk.InvocationAutonomous
	InvocationInteractive = sdk.InvocationInteractive

	ApprovalModeNever  = sdk.ApprovalModeNever
	ApprovalModeAlways = sdk.ApprovalModeAlways
	// ApprovalModeMutatingTools starts runs immediately and requires approval
	// only when a mutating tool is about to execute.
	ApprovalModeMutatingTools = "mutating_tools"
	// ApprovalModeRiskBased allows routine reversible mutations and pauses
	// before sensitive or destructive tools.
	ApprovalModeRiskBased = "risk_based"

	RunStatusQueued    = sdk.RunStatusQueued
	RunStatusRunning   = sdk.RunStatusRunning
	RunStatusPaused    = sdk.RunStatusPaused
	RunStatusCompleted = sdk.RunStatusCompleted
	RunStatusFailed    = sdk.RunStatusFailed
	RunStatusCancelled = sdk.RunStatusCancelled

	PauseReasonNone          = sdk.PauseReasonNone
	PauseReasonHumanInput    = sdk.PauseReasonHumanInput
	PauseReasonHumanApproval = sdk.PauseReasonHumanApproval
	PauseReasonAuth          = sdk.PauseReasonAuth
	PauseReasonUserMessage   = sdk.PauseReasonUserMessage

	TurnPolicyCompleteOnFinish = sdk.TurnPolicyCompleteOnFinish
	TurnPolicyPauseAfterAssist = sdk.TurnPolicyPauseAfterAssist
	TurnCompletionImplicit     = sdk.TurnCompletionImplicit
	TurnCompletionExplicit     = sdk.TurnCompletionExplicit

	ApprovalNotRequired = sdk.ApprovalNotRequired
	ApprovalPending     = sdk.ApprovalPending
	ApprovalApproved    = sdk.ApprovalApproved
	ApprovalRejected    = sdk.ApprovalRejected
)

type TargetRef = sdk.TargetRef
type TargetDisplay = sdk.TargetDisplay
type Agent = sdk.Agent
type SkillRef = sdk.SkillRef
type AgentRun = sdk.AgentRun
type RunInput = sdk.RunInput
type Usage = sdk.Usage
type TurnPolicy = sdk.TurnPolicy
type AgentRunMessage = sdk.AgentRunMessage
type AgentRunArtifact = sdk.AgentRunArtifact
type AgentRunInteraction = sdk.AgentRunInteraction
type ToolCall = sdk.ToolCall
type WorkspaceLease = sdk.WorkspaceLease
type AgentRunEvent = sdk.EventEnvelope

type RunSearch struct {
	AppID  string
	Status string
	Query  string
	Limit  int
	Offset int
}

type RunPage struct {
	Items  []AgentRun `json:"items"`
	Total  int64      `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}
