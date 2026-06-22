package skills

import (
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestSelectNativeActiveSkillsEpicDraftSpecSelectsPRDSkills(t *testing.T) {
	refs := []agentcore.SkillRef{
		{Key: "approval_protocol"},
		{Key: "prd_authorship"},
		{Key: "task_decomposition"},
		{Key: "epic_state_routing"},
		{Key: "general_agent_behavior"},
	}
	definitions := []Definition{
		{Key: "approval_protocol", SourceKind: SourceBuiltIn, Instructions: "approval"},
		{Key: "prd_authorship", SourceKind: SourceBuiltIn, Instructions: "prd"},
		{Key: "task_decomposition", SourceKind: SourceBuiltIn, Instructions: "tasks"},
		{Key: "epic_state_routing", SourceKind: SourceBuiltIn, Instructions: "routing"},
		{Key: "general_agent_behavior", SourceKind: SourceBuiltIn, Instructions: "general"},
	}

	selection := SelectNativeActiveSkills(refs, definitions, NativeActiveSelectionContext{
		PresetKey:     PresetEpicPlanner,
		TargetType:    "epic",
		PlanningStage: PlanningStageDraftSpec,
	})

	if got := testSkillKeys(selection.Refs); strings.Join(got, ",") != "approval_protocol,prd_authorship,epic_state_routing,general_agent_behavior" {
		t.Fatalf("unexpected active refs %#v", got)
	}
	if selection.Instructions != "approval\n\nprd\n\nrouting\n\ngeneral" {
		t.Fatalf("unexpected compiled instructions %q", selection.Instructions)
	}
}

func TestSelectNativeActiveSkillsKeepsWorkspaceSkillsDefaultActive(t *testing.T) {
	refs := []agentcore.SkillRef{
		{Key: "approval_protocol"},
		{SkillID: "skill-123", Key: "workspace_planner_extension"},
	}
	definitions := []Definition{
		{Key: "approval_protocol", SourceKind: SourceBuiltIn, Instructions: "approval"},
		{Key: "workspace_planner_extension", SourceKind: SourceWorkspace, Instructions: "workspace"},
	}

	selection := SelectNativeActiveSkills(refs, definitions, NativeActiveSelectionContext{
		PresetKey:     PresetTaskPlanner,
		TargetType:    "task",
		PlanningStage: PlanningStageTaskPlanDoc,
	})

	if got := testSkillKeys(selection.Refs); strings.Join(got, ",") != "approval_protocol,workspace_planner_extension" {
		t.Fatalf("unexpected active refs %#v", got)
	}
}

func TestSelectNativeActiveSkillsDocumentationSupportTargetSelectsSupportGapSkills(t *testing.T) {
	refs := SkillRefsForKeys([]string{
		"docs_information_architecture",
		"external_help_doc_writing",
		"api_doc_writing",
		"internal_docs_maintenance",
		"public_help_docs_maintenance",
		"api_docs_maintenance",
		"release_to_docs_update",
		"support_gap_to_docs",
		"general_agent_behavior",
	})
	definitions := []Definition{
		{Key: "docs_information_architecture", SourceKind: SourceBuiltIn, Instructions: "ia"},
		{Key: "external_help_doc_writing", SourceKind: SourceBuiltIn, Instructions: "help-writing"},
		{Key: "api_doc_writing", SourceKind: SourceBuiltIn, Instructions: "api-writing"},
		{Key: "internal_docs_maintenance", SourceKind: SourceBuiltIn, Instructions: "internal"},
		{Key: "public_help_docs_maintenance", SourceKind: SourceBuiltIn, Instructions: "public"},
		{Key: "api_docs_maintenance", SourceKind: SourceBuiltIn, Instructions: "api-maintenance"},
		{Key: "release_to_docs_update", SourceKind: SourceBuiltIn, Instructions: "release"},
		{Key: "support_gap_to_docs", SourceKind: SourceBuiltIn, Instructions: "gap"},
		{Key: "general_agent_behavior", SourceKind: SourceBuiltIn, Instructions: "general"},
	}

	selection := SelectNativeActiveSkills(refs, definitions, NativeActiveSelectionContext{
		PresetKey:  PresetDocumentationAgent,
		TargetType: "support_conversation",
	})

	if got := testSkillKeys(selection.Refs); strings.Join(got, ",") != "docs_information_architecture,external_help_doc_writing,public_help_docs_maintenance,support_gap_to_docs,general_agent_behavior" {
		t.Fatalf("unexpected active refs %#v", got)
	}
	if strings.Contains(selection.Instructions, "api-writing") || strings.Contains(selection.Instructions, "release") {
		t.Fatalf("unexpected inactive documentation instructions included %q", selection.Instructions)
	}
}

func testSkillKeys(refs []agentcore.SkillRef) []string {
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, ref.Key)
	}
	return keys
}
