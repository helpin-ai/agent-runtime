package skills

import (
	"testing"
	"testing/fstest"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestLoadPackageParsesSkillMarkdownAndOpenAIConfig(t *testing.T) {
	fsys := fstest.MapFS{
		"skills/approval/SKILL.md": {
			Data: []byte(`---
name: approval_protocol
description: Approval behavior
metadata:
  title: Approval Protocol
  required_tools:
    - request_approval
  supported_runtimes:
    - native_sdk
---
Always request approval before completion.
`),
		},
		"skills/approval/agents/openai.yaml": {
			Data: []byte(`interface:
  display_name: Approval
  short_description: Ask for approval
policy:
  completion_requires_interaction_kinds:
    - approval_request
  interaction_contracts:
    - kind: approval_request
      schema: approval_request_v1
      transports:
        native_sdk:
          type: tool_call
          tool_name: request_approval
`),
		},
	}

	definition, err := LoadPackage(fsys, "skills/approval", SourceBuiltIn)
	if err != nil {
		t.Fatalf("load package: %v", err)
	}
	if definition.Key != "approval_protocol" || definition.Title != "Approval" || definition.Description != "Ask for approval" {
		t.Fatalf("unexpected definition metadata: %#v", definition)
	}
	if len(definition.RequiredTools) != 1 || definition.RequiredTools[0] != "request_approval" {
		t.Fatalf("unexpected required tools: %#v", definition.RequiredTools)
	}
	if len(definition.SupportedRuntimes) != 1 || definition.SupportedRuntimes[0] != agentcore.RuntimeNativeSDK {
		t.Fatalf("unexpected supported runtimes: %#v", definition.SupportedRuntimes)
	}
	contract, ok := definition.Policy.InteractionContract(InteractionKindApprovalRequest)
	if !ok || contract.Transports[agentcore.RuntimeNativeSDK].ToolName != "request_approval" {
		t.Fatalf("expected approval interaction contract, got %#v", definition.Policy)
	}
}

func TestLoadBuiltInSkillsLoadsDirectoriesOnly(t *testing.T) {
	fsys := fstest.MapFS{
		"skills/a/SKILL.md": {
			Data: []byte("---\nname: a\ndescription: A\n---\nA instructions"),
		},
		"skills/README.md": {Data: []byte("ignored")},
	}
	definitions, err := LoadBuiltInSkills(fsys, "skills")
	if err != nil {
		t.Fatalf("load built-ins: %v", err)
	}
	if len(definitions) != 1 || definitions[0].Key != "a" {
		t.Fatalf("unexpected definitions: %#v", definitions)
	}
}
