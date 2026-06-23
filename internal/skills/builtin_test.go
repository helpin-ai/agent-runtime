package skills

import (
	"strings"
	"testing"
)

func TestLoadEmbeddedBuiltInsContainsDefaultSystemSkills(t *testing.T) {
	definitions, err := LoadEmbeddedBuiltIns()
	if err != nil {
		t.Fatalf("load embedded built-ins: %v", err)
	}
	byKey := definitionsByKey(definitions)
	for _, key := range []string{
		"approval_protocol",
		"prd_authorship",
		"task_decomposition",
		"epic_state_routing",
		"general_agent_behavior",
		"task_planner_context",
		"code_builder",
		"review_agent",
		"crm_operator",
		"support_agent",
		"dependency_auditor",
		"security_triage",
		"external_help_doc_writing",
		"api_doc_writing",
		"internal_docs_maintenance",
		"public_help_docs_maintenance",
		"api_docs_maintenance",
		"docs_information_architecture",
		"release_to_docs_update",
		"support_gap_to_docs",
	} {
		if _, ok := byKey[key]; !ok {
			t.Fatalf("expected embedded built-in skill %q", key)
		}
	}
}

func TestEmbeddedBuiltInReviewPolicyAndPrompt(t *testing.T) {
	definitions, err := LoadEmbeddedBuiltIns()
	if err != nil {
		t.Fatalf("load embedded built-ins: %v", err)
	}
	review := definitionsByKey(definitions)["review_agent"]
	if len(review.Policy.CompletionRequiresInteractionKinds) != 2 {
		t.Fatalf("expected review policy from openai.yaml, got %#v", review.Policy)
	}
	if label := ReviewCheckpointFencedBlockLabel(review.Policy, "codex"); label != "agent-runtime-review" {
		t.Fatalf("unexpected review checkpoint label %q", label)
	}

	prompt := EmbeddedBuiltInPresetPrompt(PresetReviewAgent)
	if prompt == nil || !strings.Contains(*prompt, "You are Review Agent.") || !strings.Contains(*prompt, "request_review_checkpoint") {
		t.Fatalf("unexpected review preset prompt %#v", prompt)
	}
}

func TestListEmbeddedBuiltInCatalogIncludesPresetMembership(t *testing.T) {
	catalog := ListEmbeddedBuiltInCatalog()
	for _, entry := range catalog.Skills {
		if entry.Key == "code_builder" {
			if len(entry.Presets) != 1 || entry.Presets[0] != PresetCodeBuilder {
				t.Fatalf("unexpected code_builder presets %#v", entry.Presets)
			}
			return
		}
	}
	t.Fatal("expected code_builder catalog entry")
}
