package skills

import (
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type ActiveSelectionContext struct {
	PresetKey     string
	TargetType    string
	PlanningStage string
}

type ActiveSelection struct {
	Refs         []agentcore.SkillRef
	Definitions  []Definition
	Instructions string
	Policy       Policy
}

// SelectActiveResolution narrows phase- and target-selectable built-in skills
// for every runtime adapter. Available skills remain discoverable, while the
// matching subset becomes active for policy and completion enforcement.
func SelectActiveResolution(resolution Resolution, ctx ActiveSelectionContext) Resolution {
	if resolution.UsesExplicitRoles {
		instructions := SelectActiveSkills(resolution.InstructionRefs, resolution.InstructionDefinitions, ctx)
		promoted := selectAvailableActiveSkills(resolution.AvailableRefs, resolution.AvailableDefinitions, ctx)
		resolution.InstructionRefs = append(instructions.Refs, promoted.Refs...)
		resolution.InstructionDefinitions = append(instructions.Definitions, promoted.Definitions...)
		// Explicit instruction-role skills are already compiled into the host's
		// system prompt. Only inject available-role skills promoted for this
		// target, otherwise their specialized guidance remains undiscoverable to
		// the model until it happens to load the skill itself.
		resolution.Instructions = promoted.Instructions
		resolution.Policy = AggregatePolicy(resolution.InstructionDefinitions)
		return resolution
	}
	selection := SelectActiveSkills(resolution.CoreRefs, resolution.Definitions, ctx)
	resolution.CoreRefs = selection.Refs
	resolution.Refs = NormalizeRefs(selection.Refs)
	resolution.Definitions = selection.Definitions
	resolution.Instructions = selection.Instructions
	resolution.Policy = selection.Policy
	return resolution
}

func selectAvailableActiveSkills(refs []agentcore.SkillRef, definitions []Definition, ctx ActiveSelectionContext) ActiveSelection {
	if len(refs) == 0 || len(definitions) == 0 || len(refs) != len(definitions) {
		return ActiveSelection{}
	}
	activeBuiltIns, ok := activeBuiltInSkillSet(ctx)
	if !ok {
		return ActiveSelection{}
	}
	activeRefs := make([]agentcore.SkillRef, 0, len(refs))
	activeDefs := make([]Definition, 0, len(definitions))
	for idx, ref := range refs {
		definition := definitions[idx]
		if isBuiltInDefinition(definition) && isPhaseSelectableBuiltInSkill(definition.Key) && activeBuiltIns[definition.Key] {
			activeRefs = append(activeRefs, ref)
			activeDefs = append(activeDefs, definition)
		}
	}
	return activeSelection(activeRefs, activeDefs)
}

func SelectActiveSkills(refs []agentcore.SkillRef, definitions []Definition, ctx ActiveSelectionContext) ActiveSelection {
	if len(refs) == 0 || len(definitions) == 0 || len(refs) != len(definitions) {
		return activeSelection(refs, definitions)
	}

	activeBuiltIns, ok := activeBuiltInSkillSet(ctx)
	if !ok {
		return activeSelection(refs, definitions)
	}

	activeRefs := make([]agentcore.SkillRef, 0, len(refs))
	activeDefs := make([]Definition, 0, len(definitions))
	for idx, ref := range refs {
		definition := definitions[idx]
		if shouldKeepActiveByDefault(ref, definition) || activeBuiltIns[definition.Key] {
			activeRefs = append(activeRefs, ref)
			activeDefs = append(activeDefs, definition)
		}
	}
	return activeSelection(activeRefs, activeDefs)
}

func activeSelection(refs []agentcore.SkillRef, definitions []Definition) ActiveSelection {
	return ActiveSelection{
		Refs:         append([]agentcore.SkillRef(nil), refs...),
		Definitions:  append([]Definition(nil), definitions...),
		Instructions: CompileInstructions(definitions),
		Policy:       AggregatePolicy(definitions),
	}
}

func activeBuiltInSkillSet(ctx ActiveSelectionContext) (map[string]bool, bool) {
	presetKey := strings.TrimSpace(ctx.PresetKey)
	targetType := strings.TrimSpace(ctx.TargetType)
	planningStage := strings.TrimSpace(ctx.PlanningStage)

	switch presetKey {
	case PresetEpicPlanner:
		if targetType != "epic" {
			return nil, false
		}
		switch planningStage {
		case PlanningStageDraftSpec:
			return skillKeySet(
				"approval_protocol", "prd_task_plan_approval",
				"epic_state_routing", "epic_planning_state_routing",
				"prd_authorship", "product_prd_authorship",
			), true
		case PlanningStagePlanTasks:
			return skillKeySet(
				"approval_protocol", "prd_task_plan_approval",
				"epic_state_routing", "epic_planning_state_routing",
				"task_decomposition", "coding_task_decomposition",
			), true
		default:
			return nil, false
		}
	case PresetTaskPlanner:
		if targetType != "task" {
			return nil, false
		}
		switch planningStage {
		case PlanningStageTaskPlanDoc:
			return skillKeySet(
				"approval_protocol", "prd_task_plan_approval",
				"task_planner_context", "coding_task_planning",
			), true
		default:
			return nil, false
		}
	case PresetDocumentationAgent:
		return documentationActiveBuiltInSkillSet(targetType), true
	default:
		return nil, false
	}
}

func documentationActiveBuiltInSkillSet(targetType string) map[string]bool {
	active := skillKeySet(
		"docs_information_architecture", "docs_architecture_review",
		"general_agent_behavior", "engineering_planner_operating_rules",
	)
	switch strings.TrimSpace(targetType) {
	case "document":
		addSkillKeys(active, "internal_docs_maintenance", "public_help_docs_maintenance", "api_docs_maintenance")
	case "support_conversation", "support_coverage_gap":
		addSkillKeys(active,
			"support_gap_to_docs", "support_gap_docs_update",
			"external_help_doc_writing", "public_help_doc_writing",
			"public_help_docs_maintenance",
		)
	case "repository", "task", "epic":
		addSkillKeys(active,
			"release_to_docs_update", "post_release_docs_update",
			"internal_docs_maintenance", "public_help_docs_maintenance", "api_docs_maintenance",
		)
	case "workspace":
		addAllDocumentationSkillKeys(active)
	default:
		addAllDocumentationSkillKeys(active)
	}
	return active
}

func addAllDocumentationSkillKeys(active map[string]bool) {
	addSkillKeys(active,
		"external_help_doc_writing", "public_help_doc_writing",
		"api_doc_writing", "api_reference_doc_writing",
		"internal_docs_maintenance", "public_help_docs_maintenance", "api_docs_maintenance",
		"release_to_docs_update", "post_release_docs_update",
		"support_gap_to_docs", "support_gap_docs_update",
	)
}

func skillKeySet(keys ...string) map[string]bool {
	set := make(map[string]bool, len(keys))
	addSkillKeys(set, keys...)
	return set
}

func addSkillKeys(set map[string]bool, keys ...string) {
	for _, key := range keys {
		set[key] = true
	}
}

func shouldKeepActiveByDefault(ref agentcore.SkillRef, definition Definition) bool {
	if !isBuiltInDefinition(definition) {
		return true
	}
	return !isPhaseSelectableBuiltInSkill(definition.Key)
}

func isBuiltInDefinition(definition Definition) bool {
	sourceKind := strings.TrimSpace(definition.SourceKind)
	return sourceKind == "" || sourceKind == SourceBuiltIn
}

func isPhaseSelectableBuiltInSkill(key string) bool {
	switch strings.TrimSpace(key) {
	case "approval_protocol", "prd_authorship", "task_decomposition", "epic_state_routing", "task_planner_context",
		"prd_task_plan_approval", "product_prd_authorship", "coding_task_decomposition", "epic_planning_state_routing", "coding_task_planning":
		return true
	case "docs_information_architecture", "external_help_doc_writing", "api_doc_writing", "internal_docs_maintenance", "public_help_docs_maintenance", "api_docs_maintenance", "release_to_docs_update", "support_gap_to_docs",
		"docs_architecture_review", "public_help_doc_writing", "api_reference_doc_writing", "post_release_docs_update", "support_gap_docs_update":
		return true
	default:
		return false
	}
}

// Compatibility aliases keep internal callers source-compatible while active
// selection is no longer native-runtime-specific.
type NativeActiveSelectionContext = ActiveSelectionContext
type NativeActiveSelection = ActiveSelection

func SelectNativeActiveResolution(resolution Resolution, ctx NativeActiveSelectionContext) Resolution {
	return SelectActiveResolution(resolution, ctx)
}

func SelectNativeActiveSkills(refs []agentcore.SkillRef, definitions []Definition, ctx NativeActiveSelectionContext) NativeActiveSelection {
	return SelectActiveSkills(refs, definitions, ctx)
}
