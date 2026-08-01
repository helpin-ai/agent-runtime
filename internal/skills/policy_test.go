package skills

import (
	"slices"
	"testing"
)

func TestCompletionRequiredInteractionKindsUsesPolicyAndRequiredTools(t *testing.T) {
	got := CompletionRequiredInteractionKinds(Policy{
		CompletionRequiresInteractionKinds: []string{InteractionKindReviewCheckpoint},
	}, []Definition{
		{RequiredTools: []string{"mcp__agent_runtime__request_approval"}},
		{RequiredTools: []string{"request_user_input"}},
	})

	for _, kind := range []string{
		InteractionKindApprovalRequest,
		InteractionKindRequestUserInput,
		InteractionKindReviewCheckpoint,
	} {
		if !slices.Contains(got, kind) {
			t.Fatalf("expected required interaction %q, got %v", kind, got)
		}
	}
}
