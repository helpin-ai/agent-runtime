package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type CommandToolMetadata struct {
	CommandName string
	Alias       string
	Category    string
	Description string
	InputSchema map[string]any
	Mutating    bool
}

type CommandExecutionContext struct {
	AppID             string                 `json:"app_id"`
	RunID             string                 `json:"run_id,omitempty"`
	AgentID           string                 `json:"agent_id,omitempty"`
	ExternalActorID   string                 `json:"external_actor_id,omitempty"`
	WorkspaceID       string                 `json:"workspace_id,omitempty"`
	TargetType        string                 `json:"target_type,omitempty"`
	TargetID          string                 `json:"target_id,omitempty"`
	Target            agentcore.TargetRef    `json:"target"`
	RunInputMetadata  map[string]interface{} `json:"run_input_metadata,omitempty"`
	TargetMetadata    map[string]interface{} `json:"target_metadata,omitempty"`
	WorkspaceMetadata map[string]interface{} `json:"workspace_metadata,omitempty"`
}

type CommandToolExecutor interface {
	ExecuteCommand(ctx context.Context, meta CommandExecutionContext, commandName string, input json.RawMessage) (json.RawMessage, error)
}

type CommandToolExecutorFunc func(ctx context.Context, meta CommandExecutionContext, commandName string, input json.RawMessage) (json.RawMessage, error)

func (f CommandToolExecutorFunc) ExecuteCommand(ctx context.Context, meta CommandExecutionContext, commandName string, input json.RawMessage) (json.RawMessage, error) {
	return f(ctx, meta, commandName, input)
}

func RegisterCommandTools(r *Registry, executor CommandToolExecutor, metadata []CommandToolMetadata) {
	if r == nil || executor == nil {
		return
	}
	if len(metadata) == 0 {
		metadata = AllCommandToolMetadata()
	}
	for _, meta := range metadata {
		meta := meta
		alias := CanonicalName(meta.Alias)
		if alias == "" || strings.TrimSpace(meta.CommandName) == "" {
			continue
		}
		r.Register(Definition{
			Name:        alias,
			Description: meta.Description,
			Category:    firstNonEmptyString(meta.Category, "Command"),
			InputSchema: meta.InputSchema,
			Mutating:    meta.Mutating,
		}, func(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
			output, err := executor.ExecuteCommand(ctx, CommandExecutionContextFromCallContext(callCtx), meta.CommandName, input)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", alias, err)
			}
			if len(output) == 0 {
				return json.RawMessage(`{}`), nil
			}
			return output, nil
		})
	}
}

func CommandToolMetadataForAlias(alias string) (*CommandToolMetadata, bool) {
	alias = CanonicalName(alias)
	for _, meta := range sharedCommandTools {
		if CanonicalName(meta.Alias) == alias {
			copied := meta
			return &copied, true
		}
	}
	return nil, false
}

func CommandToolMetadataForCommand(name string) (*CommandToolMetadata, bool) {
	name = strings.TrimSpace(name)
	for _, meta := range sharedCommandTools {
		if strings.TrimSpace(meta.CommandName) == name {
			copied := meta
			return &copied, true
		}
	}
	return nil, false
}

func AllCommandToolMetadata() []CommandToolMetadata {
	out := make([]CommandToolMetadata, len(sharedCommandTools))
	copy(out, sharedCommandTools)
	return out
}

func CommandExecutionContextFromCallContext(callCtx CallContext) CommandExecutionContext {
	meta := CommandExecutionContext{
		AppID:  callCtx.AppID,
		RunID:  callCtx.RunID,
		Target: callCtx.Target,
	}
	if callCtx.Agent != nil {
		meta.AgentID = callCtx.Agent.ID
		if meta.AppID == "" {
			meta.AppID = callCtx.Agent.AppID
		}
	}
	if callCtx.Run != nil {
		meta.AppID = firstNonEmptyString(meta.AppID, callCtx.Run.AppID)
		meta.RunID = firstNonEmptyString(meta.RunID, callCtx.Run.ID)
		meta.AgentID = firstNonEmptyString(meta.AgentID, callCtx.Run.AgentID)
		meta.ExternalActorID = callCtx.Run.ExternalActorID
		if meta.Target.Type == "" && meta.Target.ID == "" {
			meta.Target = callCtx.Run.Target
		}
		meta.RunInputMetadata = callCtx.Run.Input.Metadata
		if value := stringFromAnyMap(callCtx.Run.Input.Metadata, "workspace_id"); value != "" {
			meta.WorkspaceID = value
		}
		if callCtx.Run.WorkspaceLease != nil {
			meta.WorkspaceMetadata = callCtx.Run.WorkspaceLease.Metadata
			meta.WorkspaceID = firstNonEmptyString(meta.WorkspaceID, stringFromAnyMap(callCtx.Run.WorkspaceLease.Metadata, "workspace_id"))
		}
	}
	if meta.Target.Type == "" && meta.Target.ID == "" {
		meta.Target = callCtx.Target
	}
	meta.TargetType = meta.Target.Type
	meta.TargetID = meta.Target.ID
	meta.TargetMetadata = meta.Target.Metadata
	meta.WorkspaceID = firstNonEmptyString(meta.WorkspaceID, stringFromAnyMap(meta.Target.Metadata, "workspace_id"))
	return meta
}

func stringFromAnyMap(values map[string]interface{}, key string) string {
	if values == nil {
		return ""
	}
	switch value := values[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		return ""
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

var sharedCommandTools = []CommandToolMetadata{
	{
		CommandName: "workspace.list_teams",
		Alias:       "list_workspace_teams",
		Category:    "Workspace",
		Description: "List workspace teams that the agent can use for team selection, task filtering, or planning context.",
		Mutating:    false,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}, "additionalProperties": false},
	},
	{
		CommandName: "docs.ensure_spec_doc",
		Alias:       "ensure_epic_spec_doc",
		Category:    "Docs",
		Description: "Create or load the canonical product spec document for the current epic. Returns document metadata, whether an approved spec exists, the current task count, and a planning_hint for branching.",
		Mutating:    true,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		CommandName: "docs.ensure_task_plan_doc",
		Alias:       "ensure_task_plan_doc",
		Category:    "Docs",
		Description: "Create or load the canonical planning document for the current task. Returns document metadata and whether a draft already exists.",
		Mutating:    true,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		CommandName: "pm.approve_epic_spec",
		Alias:       "approve_epic_spec",
		Category:    "PM / Tasks",
		Description: "Mark the current epic spec document as approved and record the approved spec version on the epic.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"version_id": map[string]any{"type": "string", "description": "Optional existing document version ID to approve. Omit to approve the current document content."},
			},
		},
	},
	{CommandName: "pm.create_task_batch", Alias: "create_task_batch", Category: "PM / Tasks", Description: "Create implementation-ready tasks for the current epic. Supports stable refs, direct assignment, and dependency refs.", Mutating: true, InputSchema: createTaskBatchSchema()},
	{CommandName: "pm.create_task", Alias: "create_task", Category: "PM / Tasks", Description: "Create a single task for a team, optionally targeting a specific workflow and stage. If workflow_id or state_id are omitted, they are resolved from the team workflow defaults.", Mutating: true, InputSchema: createTaskSchema()},
	{CommandName: "pm.ensure_label", Alias: "ensure_task_label", Category: "PM / Tasks", Description: "Create or return a PM task label in the current workspace. Use this before creating tasks that must carry a stable label.", Mutating: true, InputSchema: ensureTaskLabelSchema()},
	{CommandName: "pm.list_tasks", Alias: "list_tasks", Category: "PM / Tasks", Description: "List tasks in the current workspace with optional label, team, open-only, description, and comment filters.", Mutating: false, InputSchema: listTasksSchema()},
	{CommandName: "pm.add_task_comment", Alias: "add_task_comment", Category: "PM / Tasks", Description: "Add a markdown comment to a task. If task_id is omitted, defaults to the current task target when available.", Mutating: true, InputSchema: addTaskCommentSchema()},
	{
		CommandName: "pm.assign_task_agent",
		Alias:       "assign_task_agent",
		Category:    "PM / Tasks",
		Description: "Deprecated. Task agent assignment was removed; use workflow automation rules or start a run explicitly with an agent.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id":  map[string]any{"type": "string", "description": "The task ID to assign"},
				"agent_id": map[string]any{"type": "string", "description": "The target agent ID"},
			},
			"required": []string{"agent_id"},
		},
	},
	{
		CommandName: "pm.set_task_dependencies",
		Alias:       "set_task_dependencies",
		Category:    "PM / Tasks",
		Description: "Create explicit task dependency links between existing tasks.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"dependencies": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"source_task_id": map[string]any{"type": "string"},
							"target_task_id": map[string]any{"type": "string"},
						},
					},
				},
			},
			"required": []string{"dependencies"},
		},
	},
	{
		CommandName: "pm.update_task_state",
		Alias:       "update_task_state",
		Category:    "PM / Tasks",
		Description: "Transition the current task to a different workflow state.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"state_id": map[string]any{"type": "string", "description": "The target workflow state ID"},
				"task_id":  map[string]any{"type": "string", "description": "Optional task ID override. Defaults to the current task target."},
			},
			"required": []string{"state_id"},
		},
	},
	{
		CommandName: "docs.write_document_content",
		Alias:       "write_document_content",
		Category:    "Docs",
		Description: "Write document content to a document. Accepts either structured document JSON or a markdown string, which will be auto-converted.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"document_id": map[string]any{"type": "string", "description": "The document ID to update"},
				"content": map[string]any{
					"description": "The document content to save. Use either a structured document JSON object or a markdown string.",
					"oneOf":       []map[string]any{{"type": "object"}, {"type": "string"}},
				},
			},
			"required": []string{"document_id", "content"},
		},
	},
	{
		CommandName: "git.list_repositories",
		Alias:       "list_repositories",
		Category:    "Git",
		Description: "List the git repositories connected to this workspace (id, full name, default branch, provider). Use this to discover a repository to target or to ask the user which repo to use.",
		Mutating:    false,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}, "additionalProperties": false},
	},
	{
		CommandName: "docs.create_document",
		Alias:       "create_document",
		Category:    "Docs",
		Description: "Create a new document. Accepts optional markdown content that will be auto-converted to rich text. If space_id is omitted it defaults to the workspace's only space; when several spaces exist, call list_spaces and ask the user which to use.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"space_id":      map[string]any{"type": "string", "description": "The space ID where the document will be created. Optional - omit to use the workspace's only space; required when multiple spaces exist (use list_spaces to discover IDs)."},
				"title":         map[string]any{"type": "string", "description": "The document title"},
				"collection_id": map[string]any{"type": "string", "description": "Optional collection ID to place the document in"},
				"content":       map[string]any{"type": "string", "description": "Optional initial document content as a markdown string. Will be auto-converted to rich text."},
				"icon":          map[string]any{"type": "string", "description": "Optional icon for the document"},
				"tags":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional tags for the document"},
			},
			"required":             []string{"title"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "docs.update_document_block",
		Alias:       "update_document_block",
		Category:    "Docs",
		Description: "Update one addressable block in a document using its current revision.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"document_id": map[string]any{"type": "string", "description": "The document ID to update"},
				"block_id":    map[string]any{"type": "string", "description": "The stable block ID to update"},
				"revision":    map[string]any{"type": "integer", "description": "The current block revision from read_document"},
				"content":     map[string]any{"type": "object", "description": "The replacement block node JSON"},
			},
			"required": []string{"document_id", "block_id", "revision", "content"},
		},
	},
	{
		CommandName: "docs.link_document_to_object",
		Alias:       "link_document_to_object",
		Category:    "Docs",
		Description: "Create a link between a document and another internal object.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"document_id":        map[string]any{"type": "string", "description": "The document ID to link"},
				"linked_object_type": map[string]any{"type": "string", "description": "The linked object type such as epic or task"},
				"linked_object_id":   map[string]any{"type": "string", "description": "The linked object ID"},
				"link_context":       map[string]any{"type": "string", "description": "Optional link context, defaults to attached"},
			},
			"required": []string{"document_id", "linked_object_type", "linked_object_id"},
		},
	},
	{
		CommandName: "crm.update_deal_stage",
		Alias:       "update_deal_stage",
		Category:    "CRM",
		Description: "Move a CRM deal to a different pipeline stage.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"deal_id":  map[string]any{"type": "string", "description": "The deal ID to update"},
				"stage_id": map[string]any{"type": "string", "description": "The target pipeline stage ID"},
			},
			"required": []string{"deal_id", "stage_id"},
		},
	},
	{
		CommandName: "crm.add_deal_note",
		Alias:       "add_deal_note",
		Category:    "CRM",
		Description: "Add a note or comment to a CRM deal.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"deal_id": map[string]any{"type": "string", "description": "The deal ID to add a note to"},
				"content": map[string]any{"type": "string", "description": "The note content"},
			},
			"required": []string{"deal_id", "content"},
		},
	},
	{CommandName: "crm.enrich_contact", Alias: "enrich_crm_contact", Category: "CRM", Description: "Safely enrich a CRM contact with sourced public data. Names and existing email/phone are protected; core fields are fill-only and agent-owned metadata is namespaced.", Mutating: true, InputSchema: crmEnrichmentSchema("contact_id", []string{"email", "phone", "job_title", "avatar_url", "linkedin_url", "location", "enrichment_note"})},
	{CommandName: "crm.enrich_company", Alias: "enrich_crm_company", Category: "CRM", Description: "Safely enrich a CRM company with sourced public data. Company name and existing domain are protected; core fields are fill-only and agent-owned metadata is namespaced.", Mutating: true, InputSchema: crmEnrichmentSchema("company_id", []string{"domain", "industry", "employee_count", "annual_revenue", "description", "logo_url", "linkedin_url", "headquarters", "enrichment_note"})},
	{
		CommandName: "crm.ensure_contact_company",
		Alias:       "ensure_crm_contact_company",
		Category:    "CRM",
		Description: "Create or reuse a CRM company and associate it with a contact. Does not modify existing company identity fields.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"contact_id":        map[string]any{"type": "string", "description": "The contact ID to associate with a company. Defaults to the current CRM contact target when omitted by the runtime."},
				"company_name":      map[string]any{"type": "string", "description": "The company name to create or match."},
				"domain":            map[string]any{"type": "string", "description": "Optional company domain to match or set on a newly-created company."},
				"source_url":        map[string]any{"type": "string", "description": "Public source URL supporting the company/contact relationship."},
				"evidence":          map[string]any{"type": "string", "description": "Short explanation of the evidence for the relationship."},
				"confidence":        map[string]any{"type": "number", "description": "Confidence from 0.0 to 1.0. Values below 0.70 are rejected.", "minimum": 0, "maximum": 1},
				"association_label": map[string]any{"type": "string", "description": "Optional association label. Defaults to primary."},
				"dry_run":           map[string]any{"type": "boolean", "description": "When true, returns whether it would create/reuse/link without writing CRM records."},
			},
			"required":             []string{"contact_id", "company_name", "source_url", "evidence", "confidence"},
			"additionalProperties": false,
		},
	},
}

func crmEnrichmentSchema(idField string, fieldEnum []string) map[string]any {
	fieldItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"field":      map[string]any{"type": "string", "enum": fieldEnum, "description": "The guarded CRM field to enrich. Identity fields such as names are intentionally unavailable."},
			"value":      map[string]any{"description": "The proposed field value. Use a string for text/url fields, an integer for employee_count, and a number for annual_revenue."},
			"source_url": map[string]any{"type": "string", "description": "Public source URL that supports the value."},
			"evidence":   map[string]any{"type": "string", "description": "Short explanation of the evidence from the source."},
			"confidence": map[string]any{"type": "number", "description": "Confidence from 0.0 to 1.0. Values below 0.70 are rejected.", "minimum": 0, "maximum": 1},
		},
		"required":             []string{"field", "value", "source_url", "evidence", "confidence"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			idField:            map[string]any{"type": "string", "description": "The CRM object ID to enrich."},
			"fields":           map[string]any{"type": "array", "description": "Guarded field updates to apply. Existing protected values are skipped, not overwritten.", "items": fieldItem, "minItems": 1, "maxItems": 20},
			"evidence_summary": map[string]any{"type": "string", "description": "Brief summary of the researched evidence."},
			"dry_run":          map[string]any{"type": "boolean", "description": "When true, returns what would be applied/skipped without writing CRM fields."},
		},
		"required":             []string{idField, "fields"},
		"additionalProperties": false,
	}
}

func createTaskBatchSchema() map[string]any {
	fileChangeSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":        map[string]any{"type": "string"},
			"action":      map[string]any{"type": "string", "enum": []string{"create", "modify", "delete"}},
			"description": map[string]any{"type": "string"},
		},
		"required":             []string{"path", "action", "description"},
		"additionalProperties": false,
	}
	implementationBriefSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"approach":        map[string]any{"type": "string"},
			"files_to_modify": map[string]any{"type": "array", "items": fileChangeSchema},
			"test_strategy":   map[string]any{"anyOf": []map[string]any{{"type": "string"}, {"type": "array", "items": map[string]any{"type": "string"}}}},
			"vertical_layers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"depends_on_files": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"required":             []string{"approach", "files_to_modify", "test_strategy"},
		"additionalProperties": false,
	}
	taskSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ref":                  map[string]any{"type": "string"},
			"name":                 map[string]any{"type": "string"},
			"description":          map[string]any{"type": "string"},
			"task_type":            map[string]any{"type": "string"},
			"estimate":             map[string]any{"type": "integer"},
			"priority":             map[string]any{"type": "string"},
			"acceptance_criteria":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"dependency_refs":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"source_refs":          map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"assign_agent_id":      map[string]any{"type": "string"},
			"slice_type":           map[string]any{"type": "string"},
			"implementation_brief": implementationBriefSchema,
		},
		"required": []string{"name", "description"},
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks":          map[string]any{"description": "The list of tasks to create.", "type": "array", "items": taskSchema},
			"proposed_tasks": map[string]any{"description": "Preferred alias for task-plan payloads. If present, it is treated the same as tasks.", "type": "array", "items": taskSchema},
		},
	}
}

func createTaskSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":             map[string]any{"type": "string", "description": "Task title"},
			"description":      map[string]any{"type": "string", "description": "Optional task description"},
			"task_type":        map[string]any{"type": "string", "description": "Optional task type such as feature, bug, or chore"},
			"estimate":         map[string]any{"type": "integer", "description": "Optional estimate value"},
			"priority":         map[string]any{"type": "string", "description": "Optional priority such as low, medium, high, or urgent"},
			"epic_id":          map[string]any{"type": "string", "description": "Optional epic ID to link the task to"},
			"team_id":          map[string]any{"type": "string", "description": "Team ID that owns the task"},
			"workflow_id":      map[string]any{"type": "string", "description": "Optional workflow ID override. Defaults to the resolved team workflow."},
			"state_id":         map[string]any{"type": "string", "description": "Optional workflow state ID override. Defaults to the resolved workflow default state."},
			"owner_member_ids": map[string]any{"type": "array", "description": "Optional workspace member IDs to assign as owners", "items": map[string]any{"type": "string"}},
			"label_ids":        map[string]any{"type": "array", "description": "Optional label IDs to attach to the task", "items": map[string]any{"type": "string"}},
			"deadline":         map[string]any{"type": "string", "description": "Optional deadline as YYYY-MM-DD or RFC3339"},
		},
		"required":             []string{"name", "team_id"},
		"additionalProperties": false,
	}
}

func ensureTaskLabelSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "Label name to create or return, for example security."},
			"team_id":     map[string]any{"type": "string", "description": "Optional team scope. Omit for a shared workspace label."},
			"description": map[string]any{"type": "string", "description": "Optional description used when the label is first created."},
			"color":       map[string]any{"type": "string", "description": "Optional hex color used when the label is first created."},
		},
		"required":             []string{"name"},
		"additionalProperties": false,
	}
}

func listTasksSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"label_id":             map[string]any{"type": "string", "description": "Optional label ID filter."},
			"team_id":              map[string]any{"type": "string", "description": "Optional team ID filter."},
			"task_id":              map[string]any{"type": "string", "description": "Optional task ID. Omit to use the current task target when available."},
			"owner_member_ids":     map[string]any{"type": "array", "description": "Optional workspace member IDs. When present, only tasks owned by at least one of these members are returned.", "items": map[string]any{"type": "string"}},
			"owned_by_actor":       map[string]any{"type": "boolean", "description": "When true, filter to tasks owned by the current workspace actor."},
			"open_only":            map[string]any{"type": "boolean", "description": "When true, only return non-completed, non-archived tasks."},
			"include_descriptions": map[string]any{"type": "boolean", "description": "When true, include task descriptions in the response."},
			"include_comments":     map[string]any{"type": "boolean", "description": "When true, include recent task comments in the response."},
			"detail_level":         map[string]any{"type": "string", "description": "Optional response shape. Use compact for bounded task rows with short description/comment excerpts.", "enum": []string{"summary", "compact", "full"}},
			"limit":                map[string]any{"type": "integer", "description": "Maximum tasks to return. Defaults to 50, max 100."},
		},
		"additionalProperties": false,
	}
}

func addTaskCommentSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task_id": map[string]any{"type": "string", "description": "Optional task ID. Omit to use the current task target when available."},
			"content": map[string]any{"type": "string", "description": "The markdown comment body."},
		},
		"required":             []string{"content"},
		"additionalProperties": false,
	}
}
