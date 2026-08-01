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

func TestCompletionRequiredInteractionKindsTreatsPublishedPlanningDraftAsApprovalPreview(t *testing.T) {
	for _, toolName := range []string{"publish_prd_draft", "publish_task_plan", "publish_task_plan_doc"} {
		t.Run(toolName, func(t *testing.T) {
			got := CompletionRequiredInteractionKinds(Policy{}, []Definition{{RequiredTools: []string{toolName}}})
			if !slices.Contains(got, InteractionKindApprovalRequest) {
				t.Fatalf("expected %s to require approval, got %v", toolName, got)
			}
		})
	}
}
