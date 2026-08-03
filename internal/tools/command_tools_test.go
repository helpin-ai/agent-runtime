package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestRegisterCommandToolsRoutesToExecutorWithRunMetadata(t *testing.T) {
	registry := NewRegistry()
	var gotName string
	var gotMeta CommandExecutionContext
	var gotInput json.RawMessage
	RegisterCommandTools(registry, CommandToolExecutorFunc(func(ctx context.Context, meta CommandExecutionContext, commandName string, input json.RawMessage) (json.RawMessage, error) {
		gotName = commandName
		gotMeta = meta
		gotInput = append(json.RawMessage(nil), input...)
		return json.RawMessage(`{"task_id":"task-1","state_id":"done"}`), nil
	}), []CommandToolMetadata{{
		CommandName: "pm.update_task_state",
		Alias:       "update_task_state",
		Category:    "PM / Tasks",
		Description: "Update task state.",
		InputSchema: map[string]any{"type": "object"},
		Mutating:    true,
	}})

	run := &agentcore.AgentRun{
		ID:              "run-1",
		AppID:           "host_app",
		AgentID:         "agent-1",
		ExternalActorID: "user-1",
		Target:          agentcore.TargetRef{Type: "task", ID: "task-1", Metadata: map[string]interface{}{"workspace_id": "ws-target"}},
		Input:           agentcore.RunInput{Metadata: map[string]interface{}{"workspace_id": "ws-run"}},
	}
	output, err := registry.Execute(context.Background(), CallContext{
		AppID: "host_app",
		RunID: "run-1",
		Agent: &agentcore.Agent{
			ID: "agent-1",
		},
		Run: run,
	}, "update_task_state", json.RawMessage(`{"state_id":"done"}`))
	if err != nil {
		t.Fatalf("execute command tool: %v", err)
	}
	if string(output) != `{"task_id":"task-1","state_id":"done"}` {
		t.Fatalf("unexpected output: %s", string(output))
	}
	if gotName != "pm.update_task_state" {
		t.Fatalf("expected command name, got %q", gotName)
	}
	if gotMeta.AppID != "host_app" || gotMeta.RunID != "run-1" || gotMeta.AgentID != "agent-1" || gotMeta.ExternalActorID != "user-1" {
		t.Fatalf("unexpected command meta identity: %#v", gotMeta)
	}
	if gotMeta.TargetType != "task" || gotMeta.TargetID != "task-1" || gotMeta.WorkspaceID != "ws-run" {
		t.Fatalf("unexpected command meta target/workspace: %#v", gotMeta)
	}
	if string(gotInput) != `{"state_id":"done"}` {
		t.Fatalf("unexpected input: %s", string(gotInput))
	}
}

func TestRegisterCommandToolsUsesSharedMetadataAndMutatingFlags(t *testing.T) {
	registry := NewRegistry()
	RegisterCommandTools(registry, CommandToolExecutorFunc(func(ctx context.Context, meta CommandExecutionContext, commandName string, input json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}), nil)

	for _, name := range []string{
		"create_task",
		"update_task_state",
		"write_document_content",
		"enrich_crm_contact",
		"draft_support_reply",
		"update_conversation_status",
		"publish_prd_draft",
		"publish_task_plan_doc",
		"publish_document_change_proposal",
	} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected command-backed tool %q", name)
		}
		if !def.Mutating {
			t.Fatalf("expected %q to be mutating", name)
		}
	}
	for _, name := range []string{
		"list_workspace_teams",
		"list_repositories",
		"list_tasks",
		"list_documents",
		"read_document",
		"get_document_blocks",
		"list_conversation_messages",
		"list_deals",
		"list_contacts",
		"list_buyer_signals",
		"search_documents",
		"get_release_context",
		"find_tasks_for_git_changes",
		"get_task_context",
	} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected command-backed tool %q", name)
		}
		if def.Mutating {
			t.Fatalf("expected %q to be read-only", name)
		}
	}
}

func TestCommandToolMetadataIncludesSharedSchemas(t *testing.T) {
	meta, ok := CommandToolMetadataForAlias("create_task_batch")
	if !ok {
		t.Fatal("expected create_task_batch metadata")
	}
	properties := meta.InputSchema["properties"].(map[string]any)
	tasks := properties["tasks"].(map[string]any)
	taskItems := tasks["items"].(map[string]any)
	taskProperties := taskItems["properties"].(map[string]any)
	brief := taskProperties["implementation_brief"].(map[string]any)
	briefProperties := brief["properties"].(map[string]any)
	if _, ok := briefProperties["files_to_modify"]; !ok {
		t.Fatalf("expected implementation_brief.files_to_modify, got %#v", briefProperties)
	}
	testStrategy := briefProperties["test_strategy"].(map[string]any)
	if _, ok := testStrategy["anyOf"].([]map[string]any); !ok {
		t.Fatalf("expected test_strategy anyOf schema, got %#v", testStrategy)
	}
}

func TestAgentCommandToolMetadataUsesSubAgentTerminology(t *testing.T) {
	for _, alias := range []string{"start_agent_run", "start_agent_plan", "get_agent_run", "cancel_agent_run", "promote_run_to_agent"} {
		meta, ok := CommandToolMetadataForAlias(alias)
		if !ok {
			t.Fatalf("missing metadata for %s", alias)
		}
		lower := strings.ToLower(meta.Description)
		if strings.Contains(lower, "child agent") || strings.Contains(lower, "child run") || strings.Contains(lower, "one-shot") {
			t.Fatalf("%s uses legacy agent terminology: %q", alias, meta.Description)
		}
		if !strings.Contains(lower, "sub-agent") {
			t.Fatalf("%s description does not use sub-agent terminology: %q", alias, meta.Description)
		}
	}
}

func TestCRMCommandToolSchemasAreStrict(t *testing.T) {
	for _, alias := range []string{"ensure_crm_contact_company", "enrich_crm_contact", "enrich_crm_company"} {
		meta, ok := CommandToolMetadataForAlias(alias)
		if !ok {
			t.Fatalf("missing metadata for %s", alias)
		}
		if got := meta.InputSchema["additionalProperties"]; got != false {
			t.Fatalf("%s additionalProperties = %#v, want false", alias, got)
		}
		required, ok := meta.InputSchema["required"].([]string)
		if !ok || len(required) == 0 {
			t.Fatalf("%s required has unexpected value %#v", alias, meta.InputSchema["required"])
		}
		if alias == "ensure_crm_contact_company" {
			continue
		}
		properties := meta.InputSchema["properties"].(map[string]any)
		fields := properties["fields"].(map[string]any)
		item := fields["items"].(map[string]any)
		if got := item["additionalProperties"]; got != false {
			t.Fatalf("%s field item additionalProperties = %#v, want false", alias, got)
		}
	}
}

func TestAllCommandToolMetadataIncludesSharedCommandSet(t *testing.T) {
	aliases := map[string]bool{}
	for _, meta := range AllCommandToolMetadata() {
		aliases[meta.Alias] = true
		if strings.TrimSpace(meta.CommandName) == "" {
			t.Fatalf("command metadata missing command name: %#v", meta)
		}
	}
	for _, alias := range []string{
		"list_workspace_teams",
		"list_documents",
		"read_document",
		"get_document_blocks",
		"ensure_epic_spec_doc",
		"create_task_batch",
		"list_repositories",
		"create_document",
		"update_deal_stage",
		"ensure_crm_contact_company",
		"list_conversation_messages",
		"draft_support_reply",
		"update_conversation_status",
		"list_deals",
		"list_contacts",
		"list_buyer_signals",
		"search_documents",
		"get_release_context",
		"find_tasks_for_git_changes",
		"get_task_context",
		"publish_prd_draft",
		"publish_task_plan_doc",
		"publish_document_change_proposal",
	} {
		if !aliases[alias] {
			t.Fatalf("expected shared command metadata for %q", alias)
		}
	}
}

func TestProductToolCommandAliasesMapToInternalCommands(t *testing.T) {
	tests := []struct {
		alias   string
		command string
	}{
		{alias: "list_conversation_messages", command: "support.list_conversation_messages"},
		{alias: "draft_support_reply", command: "support.draft_reply"},
		{alias: "update_conversation_status", command: "support.update_conversation_status"},
		{alias: "list_deals", command: "crm.list_deals"},
		{alias: "list_contacts", command: "crm.list_contacts"},
		{alias: "list_buyer_signals", command: "crm.list_buyer_signals"},
		{alias: "search_documents", command: "docs.search_documents"},
		{alias: "get_release_context", command: "release.get_release_context"},
		{alias: "find_tasks_for_git_changes", command: "release.find_tasks_for_git_changes"},
		{alias: "get_task_context", command: "release.get_task_context"},
		{alias: "publish_prd_draft", command: "docs.publish_prd_draft"},
		{alias: "publish_task_plan_doc", command: "docs.publish_task_plan_doc"},
		{alias: "publish_document_change_proposal", command: "docs.publish_document_change_proposal"},
	}
	for _, tt := range tests {
		meta, ok := CommandToolMetadataForAlias(tt.alias)
		if !ok {
			t.Fatalf("missing metadata for alias %q", tt.alias)
		}
		if meta.CommandName != tt.command {
			t.Fatalf("alias %q maps to %q, want %q", tt.alias, meta.CommandName, tt.command)
		}
		if byCommand, ok := CommandToolMetadataForCommand(tt.command); !ok || byCommand.Alias != tt.alias {
			t.Fatalf("command %q lookup returned %#v", tt.command, byCommand)
		}
		if meta.InputSchema == nil {
			t.Fatalf("alias %q missing input schema", tt.alias)
		}
	}
}
