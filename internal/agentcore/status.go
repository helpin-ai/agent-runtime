package agentcore

import "strings"

func NormalizeRun(run *AgentRun) {
	if run == nil {
		return
	}
	if run.Status == "" {
		run.Status = RunStatusQueued
	}
	if run.PauseReason == "" {
		run.PauseReason = PauseReasonNone
	}
	if run.ApprovalState == "" {
		run.ApprovalState = ApprovalNotRequired
	}
	if run.RuntimeKind == "" {
		run.RuntimeKind = RuntimeNativeSDK
	}
	if run.InvocationMode == "" {
		run.InvocationMode = InvocationAutonomous
	}
	run.Input.TurnPolicy = NormalizeTurnPolicy(run.Input.TurnPolicy)
}

func IsTerminalStatus(status string) bool {
	switch status {
	case RunStatusCompleted, RunStatusFailed, RunStatusCancelled:
		return true
	default:
		return false
	}
}

func IsActiveStatus(status string) bool {
	switch status {
	case RunStatusQueued, RunStatusRunning, RunStatusPaused:
		return true
	default:
		return false
	}
}

func InitialApprovalState(agent *Agent) string {
	if agent != nil && agent.ApprovalMode == ApprovalModeAlways {
		return ApprovalPending
	}
	return ApprovalNotRequired
}

func NormalizeTurnPolicy(policy TurnPolicy) TurnPolicy {
	policy.Mode = strings.TrimSpace(policy.Mode)
	policy.ExpiredResumeStrategy = strings.TrimSpace(policy.ExpiredResumeStrategy)
	if policy.Mode == "" {
		policy.Mode = TurnPolicyCompleteOnFinish
	}
	if policy.Mode != TurnPolicyPauseAfterAssist {
		policy.Mode = TurnPolicyCompleteOnFinish
		policy.IdleTimeoutSeconds = 0
		policy.ExpiredResumeStrategy = ""
		return policy
	}
	if policy.IdleTimeoutSeconds < 0 {
		policy.IdleTimeoutSeconds = 0
	}
	return policy
}

func ShouldPauseAfterAssistant(run *AgentRun) bool {
	if run == nil {
		return false
	}
	return NormalizeTurnPolicy(run.Input.TurnPolicy).Mode == TurnPolicyPauseAfterAssist
}
