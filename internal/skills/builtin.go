package skills

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const BuiltInRoot = "system"

const (
	PresetEpicPlanner        = "epic_planner"
	PresetTaskPlanner        = "task_planner"
	PresetCRMOperator        = "crm_operator"
	PresetSupportAgent       = "support_agent"
	PresetDocumentationAgent = "documentation_agent"
	PresetCodeBuilder        = "code_builder"
	PresetReviewAgent        = "review_agent"
	PresetCommandAgent       = "researcher"

	PlanningStageDraftSpec   = "draft_spec"
	PlanningStagePlanTasks   = "plan_tasks"
	PlanningStageTaskPlanDoc = "task_plan_doc"
)

//go:embed system
var BuiltInFS embed.FS

type PresetSkillBundle struct {
	Preamble  string
	SkillKeys []string
}

type CatalogEntry struct {
	Key               string   `json:"key"`
	Title             string   `json:"title"`
	Description       string   `json:"description"`
	Instructions      string   `json:"instructions"`
	SourceKind        string   `json:"source_kind"`
	RequiredTools     []string `json:"required_tools,omitempty"`
	SupportedRuntimes []string `json:"supported_runtimes,omitempty"`
	Presets           []string `json:"presets,omitempty"`
}

type Catalog struct {
	Skills []CatalogEntry `json:"skills"`
}

func LoadEmbeddedBuiltIns() ([]Definition, error) {
	return LoadBuiltInSkills(BuiltInFS, BuiltInRoot)
}

func MustLoadEmbeddedBuiltIns() []Definition {
	definitions, err := LoadEmbeddedBuiltIns()
	if err != nil {
		panic(fmt.Sprintf("load built-in skills: %v", err))
	}
	return definitions
}

func NewDefaultRegistry() *Registry {
	return NewRegistry(MustLoadEmbeddedBuiltIns()...)
}

func BuiltInPresetSkillBundleForPreset(presetKey string) (PresetSkillBundle, bool) {
	bundle, ok := builtInPresetSkillBundles[strings.TrimSpace(presetKey)]
	if !ok {
		return PresetSkillBundle{}, false
	}
	bundle.SkillKeys = append([]string(nil), bundle.SkillKeys...)
	return bundle, true
}

func ListCatalog(definitions []Definition) Catalog {
	byKey := definitionsByKey(definitions)
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	entries := make([]CatalogEntry, 0, len(keys))
	for _, key := range keys {
		definition := byKey[key]
		presets := presetsForSkill(key)
		entries = append(entries, CatalogEntry{
			Key:               definition.Key,
			Title:             definition.Title,
			Description:       definition.Description,
			Instructions:      definition.Instructions,
			SourceKind:        definition.SourceKind,
			RequiredTools:     append([]string(nil), definition.RequiredTools...),
			SupportedRuntimes: append([]string(nil), definition.SupportedRuntimes...),
			Presets:           presets,
		})
	}
	return Catalog{Skills: entries}
}

func ListEmbeddedBuiltInCatalog() Catalog {
	return ListCatalog(MustLoadEmbeddedBuiltIns())
}

func CompileInstructionModules(definitions []Definition, moduleKeys []string) string {
	byKey := definitionsByKey(definitions)
	sections := make([]string, 0, len(moduleKeys))
	for _, key := range moduleKeys {
		definition, ok := byKey[strings.TrimSpace(key)]
		if !ok {
			continue
		}
		instructions := RenderRuntimeToolNamesInInstructions(definition.Instructions)
		if instructions == "" {
			continue
		}
		sections = append(sections, instructions)
	}
	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func CompilePresetInstructions(definitions []Definition, preamble string, moduleKeys []string) string {
	sections := make([]string, 0, len(moduleKeys)+1)
	if strings.TrimSpace(preamble) != "" {
		sections = append(sections, strings.TrimSpace(preamble))
	}
	if compiled := CompileInstructionModules(definitions, moduleKeys); compiled != "" {
		sections = append(sections, compiled)
	}
	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func EmbeddedBuiltInPresetPrompt(presetKey string) *string {
	bundle, ok := BuiltInPresetSkillBundleForPreset(presetKey)
	if !ok {
		return nil
	}
	compiled := CompilePresetInstructions(MustLoadEmbeddedBuiltIns(), bundle.Preamble, bundle.SkillKeys)
	return &compiled
}

func InstructionTemplateVersionForPreset(definitions []Definition, preamble string, moduleKeys []string) string {
	compiled := CompilePresetInstructions(definitions, preamble, moduleKeys)
	sum := sha256.Sum256([]byte(compiled))
	return hex.EncodeToString(sum[:])[:12]
}

func EmbeddedBuiltInPresetInstructionTemplateVersion(presetKey string) string {
	bundle, ok := BuiltInPresetSkillBundleForPreset(presetKey)
	if !ok {
		return ""
	}
	return InstructionTemplateVersionForPreset(MustLoadEmbeddedBuiltIns(), bundle.Preamble, bundle.SkillKeys)
}

func RenderRuntimeToolNamesInInstructions(instructions string) string {
	rendered := strings.TrimSpace(instructions)
	if rendered == "" {
		return ""
	}
	for _, alias := range agentRuntimeMCPToolAliases() {
		runtimeName := agentRuntimeMCPToolName(alias)
		if runtimeName == "" || runtimeName == alias {
			continue
		}
		rendered = strings.ReplaceAll(rendered, "`"+alias+"`", "`"+runtimeName+"`")
	}
	return rendered
}

func agentRuntimeMCPToolName(alias string) string {
	canonical := tools.CanonicalName(alias)
	if canonical == "" {
		return ""
	}
	if strings.HasPrefix(canonical, "mcp__agent_runtime__") {
		return canonical
	}
	if !isAgentRuntimeMCPToolAlias(canonical) {
		return canonical
	}
	return "mcp__agent_runtime__" + canonical
}

func isAgentRuntimeMCPToolAlias(name string) bool {
	switch tools.CanonicalName(name) {
	case "update_plan",
		"request_user_input",
		"request_approval",
		"request_review_checkpoint",
		"publish_preview",
		"preview_markdown",
		"preview_json",
		"publish_prd_draft",
		"publish_task_plan",
		"publish_task_plan_doc",
		"publish_document_change_proposal",
		"publish_ai_section_candidate",
		"add_task_comment",
		"list_task_checklist",
		"list_epic_tasks",
		"list_workspace_teams",
		"list_team_workflows_with_stages",
		"create_task",
		"list_documents",
		"list_collections",
		"read_document",
		"get_document_blocks",
		"search_documents",
		"create_document",
		"list_deals",
		"list_contacts",
		"list_buyer_signals":
		return true
	default:
		return false
	}
}

func agentRuntimeMCPToolAliases() []string {
	aliases := []string{
		"update_plan",
		"request_user_input",
		"request_approval",
		"request_review_checkpoint",
		"publish_preview",
		"preview_markdown",
		"preview_json",
		"publish_prd_draft",
		"publish_task_plan",
		"publish_task_plan_doc",
		"publish_document_change_proposal",
		"publish_ai_section_candidate",
		"add_task_comment",
		"list_task_checklist",
		"list_epic_tasks",
		"list_workspace_teams",
		"list_team_workflows_with_stages",
		"create_task",
		"list_documents",
		"list_collections",
		"read_document",
		"get_document_blocks",
		"search_documents",
		"create_document",
		"list_deals",
		"list_contacts",
		"list_buyer_signals",
	}
	aliases = NormalizeToolNames(aliases)
	sort.SliceStable(aliases, func(i, j int) bool {
		return len(aliases[i]) > len(aliases[j])
	})
	return aliases
}

func definitionsByKey(definitions []Definition) map[string]Definition {
	byKey := make(map[string]Definition, len(definitions))
	for _, definition := range definitions {
		if key := strings.TrimSpace(definition.Key); key != "" {
			byKey[key] = definition
		}
	}
	return byKey
}

func presetsForSkill(skillKey string) []string {
	presets := make([]string, 0, len(builtInPresetSkillBundles))
	for presetKey, bundle := range builtInPresetSkillBundles {
		for _, key := range bundle.SkillKeys {
			if strings.TrimSpace(key) == strings.TrimSpace(skillKey) {
				presets = append(presets, presetKey)
				break
			}
		}
	}
	sort.Strings(presets)
	return presets
}

var builtInPresetSkillBundles = map[string]PresetSkillBundle{
	PresetEpicPlanner: {
		Preamble:  "You are Epic Planner. You run the full PRD-to-tasks loop inside a single interactive agent run.",
		SkillKeys: []string{"approval_protocol", "prd_authorship", "task_decomposition", "epic_state_routing", "general_agent_behavior"},
	},
	PresetTaskPlanner: {
		Preamble:  "You are Task Planner. Run a single interactive planning conversation for one task.",
		SkillKeys: []string{"task_planner_context", "approval_protocol", "general_agent_behavior"},
	},
	PresetCRMOperator: {
		Preamble:  "You are CRM Operator.",
		SkillKeys: []string{"crm_operator"},
	},
	PresetSupportAgent: {
		Preamble:  "You are Support Agent.",
		SkillKeys: []string{"support_agent"},
	},
	PresetDocumentationAgent: {
		Preamble: "You are Documentation Agent. Keep the current workspace's internal docs, public help docs, and API docs accurate, organized, and current. First identify the documentation surface: internal docs, public help center, API docs, or multiple surfaces. If the surface or source of truth is ambiguous, ask for clarification before changing docs. Use the workspace name from runtime context when a product, company, or workspace name is needed. Prefer drafts, proposals, and review checkpoints before customer-facing publication.",
		SkillKeys: []string{
			"docs_information_architecture",
			"external_help_doc_writing",
			"api_doc_writing",
			"internal_docs_maintenance",
			"public_help_docs_maintenance",
			"api_docs_maintenance",
			"release_to_docs_update",
			"support_gap_to_docs",
			"general_agent_behavior",
		},
	},
	PresetCodeBuilder: {
		Preamble:  "You are Code Builder. Use the relevant available engineering instructions and skills for the task, then make focused, reviewable progress in the repository.",
		SkillKeys: []string{"code_builder"},
	},
	PresetReviewAgent: {
		Preamble:  "You are Review Agent. Use the relevant available review instructions and skills for the task, and prioritize clear findings, risks, and verification gaps.",
		SkillKeys: []string{"review_agent"},
	},
}

func SkillRefsForKeys(keys []string) []agentcore.SkillRef {
	refs := make([]agentcore.SkillRef, 0, len(keys))
	for _, key := range keys {
		if key = strings.TrimSpace(key); key != "" {
			refs = append(refs, agentcore.SkillRef{Key: key})
		}
	}
	return refs
}
