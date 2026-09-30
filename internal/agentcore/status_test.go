package agentcore

import (
	"strings"
	"testing"
)

func TestInitialApprovalStateByMode(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want string
	}{
		{name: "no approval", mode: ApprovalModeNever, want: ApprovalNotRequired},
		{name: "mutating tools", mode: ApprovalModeMutatingTools, want: ApprovalNotRequired},
		{name: "before run and tools", mode: ApprovalModeAlways, want: ApprovalPending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InitialApprovalState(&Agent{ApprovalMode: tt.mode}); got != tt.want {
				t.Fatalf("InitialApprovalState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeTurnPolicyDefaultsExplicitCompletionCorrections(t *testing.T) {
	policy := NormalizeTurnPolicy(TurnPolicy{
		Mode:           TurnPolicyPauseAfterAssist,
		CompletionMode: TurnCompletionExplicit,
	})
	if policy.CompletionMode != TurnCompletionExplicit || policy.MaxCompletionCorrections != 2 {
		t.Fatalf("NormalizeTurnPolicy() = %#v", policy)
	}
}

func TestValidateTurnPolicyPreservesLegacyAndRejectsUnsupportedExplicitRuntime(t *testing.T) {
	if err := ValidateTurnPolicy(TurnPolicy{Mode: TurnPolicyPauseAfterAssist}, RuntimeOpenCode); err != nil {
		t.Fatalf("legacy policy should remain valid: %v", err)
	}
	if err := ValidateTurnPolicy(TurnPolicy{CompletionMode: TurnCompletionExplicit}, RuntimeA2A); err != nil {
		t.Fatalf("a2a turns finish explicitly when the remote task completes: %v", err)
	}
	err := ValidateTurnPolicy(TurnPolicy{CompletionMode: TurnCompletionExplicit}, RuntimeOpenCode)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected unsupported runtime error, got %v", err)
	}
}
