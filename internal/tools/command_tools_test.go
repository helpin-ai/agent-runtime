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

	for _, name := range []string{"create_task", "update_task_state", "write_document_content", "enrich_crm_contact"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected command-backed tool %q", name)
		}
		if !def.Mutating {
			t.Fatalf("expected %q to be mutating", name)
		}
	}
	for _, name := range []string{"list_workspace_teams", "list_repositories", "list_tasks", "list_documents", "read_document", "get_document_blocks"} {
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
	} {
		if !aliases[alias] {
			t.Fatalf("expected shared command metadata for %q", alias)
		}
	}
}
