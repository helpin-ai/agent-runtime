package agentcore

import (
	"fmt"
	"strings"
)

const (
	defaultCompletionCorrections = 2
	maximumCompletionCorrections = 5
)

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
	policy.CompletionMode = strings.TrimSpace(policy.CompletionMode)
	if policy.Mode == "" {
		policy.Mode = TurnPolicyCompleteOnFinish
	}
	if policy.Mode != TurnPolicyPauseAfterAssist {
		policy.Mode = TurnPolicyCompleteOnFinish
		policy.IdleTimeoutSeconds = 0
		policy.ExpiredResumeStrategy = ""
	} else if policy.IdleTimeoutSeconds < 0 {
		policy.IdleTimeoutSeconds = 0
	}
	if policy.CompletionMode != TurnCompletionExplicit {
		policy.CompletionMode = TurnCompletionImplicit
		policy.MaxCompletionCorrections = 0
	} else if policy.MaxCompletionCorrections == 0 {
		policy.MaxCompletionCorrections = defaultCompletionCorrections
	}
	return policy
}

// ValidateTurnPolicy rejects unsupported completion contracts before they can
// be normalized into a weaker policy.
func ValidateTurnPolicy(policy TurnPolicy, runtimeKind string) error {
	completionMode := strings.TrimSpace(policy.CompletionMode)
	switch completionMode {
	case "", TurnCompletionImplicit:
		if policy.MaxCompletionCorrections != 0 {
			return fmt.Errorf("max_completion_corrections requires completion_mode %q", TurnCompletionExplicit)
		}
		return nil
	case TurnCompletionExplicit:
		switch strings.TrimSpace(runtimeKind) {
		case RuntimeNativeSDK, RuntimeCodex:
		default:
			return fmt.Errorf("completion_mode %q is not supported by runtime %q", completionMode, runtimeKind)
		}
		if policy.MaxCompletionCorrections < 0 || policy.MaxCompletionCorrections > maximumCompletionCorrections {
			return fmt.Errorf("max_completion_corrections must be between 0 and %d", maximumCompletionCorrections)
		}
		return nil
	default:
		return fmt.Errorf("unsupported completion_mode %q", completionMode)
	}
}

// RequiresExplicitTurnFinish reports whether the current assistant turn must
// use the runtime-owned finish_turn control tool before it may end.
func RequiresExplicitTurnFinish(run *AgentRun) bool {
	if run == nil {
		return false
	}
	return NormalizeTurnPolicy(run.Input.TurnPolicy).CompletionMode == TurnCompletionExplicit
}

func ShouldPauseAfterAssistant(run *AgentRun) bool {
	if run == nil {
		return false
	}
	return NormalizeTurnPolicy(run.Input.TurnPolicy).Mode == TurnPolicyPauseAfterAssist
}
