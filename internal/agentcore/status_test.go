package agentcore

import "testing"

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
