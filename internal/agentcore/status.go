package agentcore

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
