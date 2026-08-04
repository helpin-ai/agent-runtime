package skills

import (
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type NativeActiveSelectionContext struct {
	PresetKey     string
	TargetType    string
	PlanningStage string
}

type NativeActiveSelection struct {
	Refs         []agentcore.SkillRef
	Definitions  []Definition
	Instructions string
	Policy       Policy
}

func SelectNativeActiveResolution(resolution Resolution, ctx NativeActiveSelectionContext) Resolution {
	if resolution.UsesExplicitRoles {
		selection := SelectNativeActiveSkills(resolution.InstructionRefs, resolution.InstructionDefinitions, ctx)
		resolution.InstructionRefs = selection.Refs
		resolution.InstructionDefinitions = selection.Definitions
		resolution.CoreRefs = append(append([]agentcore.SkillRef(nil), selection.Refs...), resolution.AvailableRefs...)
		resolution.Definitions = append(append([]Definition(nil), selection.Definitions...), resolution.AvailableDefinitions...)
		resolution.Refs = NormalizeRefs(resolution.CoreRefs)
		resolution.Instructions = ""
		resolution.Policy = selection.Policy
		return resolution
	}
	selection := SelectNativeActiveSkills(resolution.CoreRefs, resolution.Definitions, ctx)
	resolution.CoreRefs = selection.Refs
	resolution.Refs = NormalizeRefs(selection.Refs)
	resolution.Definitions = selection.Definitions
	resolution.Instructions = selection.Instructions
	resolution.Policy = selection.Policy
	return resolution
}

func SelectNativeActiveSkills(refs []agentcore.SkillRef, definitions []Definition, ctx NativeActiveSelectionContext) NativeActiveSelection {
	if len(refs) == 0 || len(definitions) == 0 || len(refs) != len(definitions) {
		return nativeActiveSelection(refs, definitions)
	}

	activeBuiltIns, ok := activeBuiltInSkillSet(ctx)
	if !ok {
		return nativeActiveSelection(refs, definitions)
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
	return nativeActiveSelection(activeRefs, activeDefs)
}

func nativeActiveSelection(refs []agentcore.SkillRef, definitions []Definition) NativeActiveSelection {
	return NativeActiveSelection{
		Refs:         append([]agentcore.SkillRef(nil), refs...),
		Definitions:  append([]Definition(nil), definitions...),
		Instructions: CompileInstructions(definitions),
		Policy:       AggregatePolicy(definitions),
	}
}

func activeBuiltInSkillSet(ctx NativeActiveSelectionContext) (map[string]bool, bool) {
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
			return map[string]bool{
				"approval_protocol":  true,
				"epic_state_routing": true,
				"prd_authorship":     true,
			}, true
		case PlanningStagePlanTasks:
			return map[string]bool{
				"approval_protocol":  true,
				"epic_state_routing": true,
				"task_decomposition": true,
			}, true
		default:
			return nil, false
		}
	case PresetTaskPlanner:
		if targetType != "task" {
			return nil, false
		}
		switch planningStage {
		case PlanningStageTaskPlanDoc:
			return map[string]bool{
				"approval_protocol":    true,
				"task_planner_context": true,
			}, true
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
	active := map[string]bool{
		"docs_information_architecture": true,
		"general_agent_behavior":        true,
	}
	switch strings.TrimSpace(targetType) {
	case "document":
		active["internal_docs_maintenance"] = true
		active["public_help_docs_maintenance"] = true
		active["api_docs_maintenance"] = true
	case "support_conversation", "support_coverage_gap":
		active["support_gap_to_docs"] = true
		active["external_help_doc_writing"] = true
		active["public_help_docs_maintenance"] = true
	case "repository", "task", "epic":
		active["release_to_docs_update"] = true
		active["internal_docs_maintenance"] = true
		active["public_help_docs_maintenance"] = true
		active["api_docs_maintenance"] = true
	case "workspace":
		active["external_help_doc_writing"] = true
		active["api_doc_writing"] = true
		active["internal_docs_maintenance"] = true
		active["public_help_docs_maintenance"] = true
		active["api_docs_maintenance"] = true
		active["release_to_docs_update"] = true
		active["support_gap_to_docs"] = true
	default:
		active["external_help_doc_writing"] = true
		active["api_doc_writing"] = true
		active["internal_docs_maintenance"] = true
		active["public_help_docs_maintenance"] = true
		active["api_docs_maintenance"] = true
		active["release_to_docs_update"] = true
		active["support_gap_to_docs"] = true
	}
	return active
}

func shouldKeepActiveByDefault(ref agentcore.SkillRef, definition Definition) bool {
	if strings.TrimSpace(ref.SkillID) != "" {
		return true
	}
	if strings.TrimSpace(definition.SourceKind) != "" && strings.TrimSpace(definition.SourceKind) != SourceBuiltIn {
		return true
	}
	return !isPhaseSelectableBuiltInSkill(definition.Key)
}

func isPhaseSelectableBuiltInSkill(key string) bool {
	switch strings.TrimSpace(key) {
	case "approval_protocol", "prd_authorship", "task_decomposition", "epic_state_routing", "task_planner_context":
		return true
	case "docs_information_architecture", "external_help_doc_writing", "api_doc_writing", "internal_docs_maintenance", "public_help_docs_maintenance", "api_docs_maintenance", "release_to_docs_update", "support_gap_to_docs":
		return true
	default:
		return false
	}
}
