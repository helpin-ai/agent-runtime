package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime-go"
)

type CommandToolMetadata struct {
	CommandName string
	Alias       string
	Category    string
	Description string
	InputSchema map[string]any
	Mutating    bool
	RiskLevel   string
}

type CommandExecutionContext = sdk.CommandExecutionContext

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
			RiskLevel:   commandToolRiskLevel(meta),
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

func commandToolRiskLevel(meta CommandToolMetadata) string {
	if !meta.Mutating {
		return RiskLevelRead
	}
	if level := strings.TrimSpace(meta.RiskLevel); level != "" {
		return level
	}
	if level, ok := commandToolRiskLevels[CanonicalName(meta.Alias)]; ok {
		return level
	}
	return RiskLevelSensitive
}

var commandToolRiskLevels = map[string]string{
	"create_space": RiskLevelRoutine, "create_collection": RiskLevelRoutine,
	"create_document": RiskLevelRoutine, "update_space": RiskLevelRoutine,
	"update_collection": RiskLevelRoutine, "move_document": RiskLevelRoutine,
	"write_document_content": RiskLevelRoutine, "update_document_block": RiskLevelRoutine,
	"link_document_to_object": RiskLevelRoutine, "ensure_epic_spec_doc": RiskLevelRoutine,
	"ensure_task_plan_doc": RiskLevelRoutine, "publish_document_change_proposal": RiskLevelRoutine,
	"publish_ai_section_candidate": RiskLevelRoutine,
	"create_task":                  RiskLevelRoutine, "create_task_batch": RiskLevelRoutine,
	"create_task_checklist_item": RiskLevelRoutine, "update_task_checklist_item": RiskLevelRoutine,
	"update_task": RiskLevelRoutine, "update_task_state": RiskLevelRoutine,
	"update_story_state": RiskLevelRoutine, "set_task_dependencies": RiskLevelRoutine,
	"ensure_task_label": RiskLevelRoutine, "add_task_comment": RiskLevelRoutine,
	"create_epic": RiskLevelRoutine, "update_epic": RiskLevelRoutine,
	"create_sprint": RiskLevelRoutine, "update_sprint": RiskLevelRoutine,
	"create_objective": RiskLevelRoutine, "update_objective": RiskLevelRoutine,
	"add_deal_note": RiskLevelRoutine, "update_deal_stage": RiskLevelRoutine,
	"ensure_crm_contact_company": RiskLevelRoutine, "enrich_crm_contact": RiskLevelRoutine,
	"enrich_crm_company": RiskLevelRoutine, "draft_support_reply": RiskLevelRoutine,
	"update_conversation_status": RiskLevelRoutine,
	"start_agent_run":            RiskLevelRoutine, "start_agent_plan": RiskLevelRoutine,
	"cancel_agent_run":   RiskLevelRoutine,
	"send_support_reply": RiskLevelSensitive, "escalate_to_human": RiskLevelSensitive,
	"run_epic_delivery_pipeline": RiskLevelDestructive,
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
		CommandName: "docs.list_documents",
		Alias:       "list_documents",
		Category:    "Docs",
		Description: "List documents in the current workspace. Use status=draft for questions about documents that need to be published.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"space_id":         map[string]any{"type": "string", "description": "Optional Docs space ID filter."},
				"collection_id":    map[string]any{"type": "string", "description": "Optional collection ID filter."},
				"team_id":          map[string]any{"type": "string", "description": "Optional team ID filter."},
				"status":           map[string]any{"type": "string", "description": "Optional document status filter. Use draft for documents that need publishing.", "enum": []string{"draft", "published", "archived"}},
				"include_archived": map[string]any{"type": "boolean", "description": "When true, include archived documents when status is omitted."},
				"limit":            map[string]any{"type": "integer", "description": "Maximum documents to return. Defaults to 50, max 100."},
			},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "docs.read_document",
		Alias:       "read_document",
		Category:    "Docs",
		Description: "Read a known document by ID. Returns metadata, a bounded plain-text excerpt, and the first page of compact addressable blocks.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"document_id": map[string]any{"type": "string", "description": "The document ID to read. Defaults to the current document target when omitted."},
			},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "docs.get_document_blocks",
		Alias:       "get_document_blocks",
		Category:    "Docs",
		Description: "Fetch addressable blocks for a known document. Use after read_document when more document context is needed.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"document_id":     map[string]any{"type": "string", "description": "The document ID whose blocks should be fetched. Defaults to the current document target when omitted."},
				"block_ids":       map[string]any{"type": "array", "description": "Optional stable block IDs to fetch.", "items": map[string]any{"type": "string"}},
				"include_content": map[string]any{"type": "boolean", "description": "When true, include full block node JSON. Limited to 20 blocks per call."},
				"offset":          map[string]any{"type": "integer", "description": "Optional zero-based block offset for paging."},
				"limit":           map[string]any{"type": "integer", "description": "Optional page size, default 40, max 100."},
				"anchor_block_id": map[string]any{"type": "string", "description": "Optional block ID to center a window around."},
				"around":          map[string]any{"type": "integer", "description": "Optional number of sibling blocks before and after anchor_block_id, default 5, max 25."},
			},
			"additionalProperties": false,
		},
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
	{
		CommandName: "support.list_conversation_messages",
		Alias:       "list_conversation_messages",
		Category:    "Support",
		Description: "List the current support conversation messages.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"conversation_id": map[string]any{"type": "string", "description": "Optional conversation ID. Defaults to the current conversation target."},
			},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "support.draft_reply",
		Alias:       "draft_support_reply",
		Category:    "Support",
		Description: "Draft a support reply for later human approval.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content":             map[string]any{"type": "string", "description": "The reply content to send after approval"},
				"is_internal":         map[string]any{"type": "boolean", "description": "Whether this should be saved as an internal-only note"},
				"sender_display_name": map[string]any{"type": "string", "description": "Optional display name for the drafted response"},
			},
			"required": []string{"content"},
		},
	},
	{
		CommandName: "support.update_conversation_status",
		Alias:       "update_conversation_status",
		Category:    "Support",
		Description: "Transition the current support conversation to a different status.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status":          map[string]any{"type": "string", "description": "The target conversation status"},
				"conversation_id": map[string]any{"type": "string", "description": "Optional conversation ID. Defaults to the current conversation target."},
			},
			"required": []string{"status"},
		},
	},
	{
		CommandName: "crm.list_deals",
		Alias:       "list_deals",
		Category:    "CRM",
		Description: "List CRM deals in the workspace. Returns deal name, stage, and amount.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer", "description": "Maximum number of deals to return (default 20, max 50)"},
			},
		},
	},
	{
		CommandName: "crm.list_contacts",
		Alias:       "list_contacts",
		Category:    "CRM",
		Description: "List CRM contacts in the workspace. Returns name, email, and job title.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer", "description": "Maximum number of contacts to return (default 20, max 50)"},
			},
		},
	},
	{
		CommandName: "crm.list_buyer_signals",
		Alias:       "list_buyer_signals",
		Category:    "CRM",
		Description: "List detected buyer signals from emails, meetings, and support conversations.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"deal_id": map[string]any{"type": "string", "description": "Optional deal ID to filter signals for a specific deal"},
				"limit":   map[string]any{"type": "integer", "description": "Maximum number of signals to return (default 20, max 50)"},
			},
		},
	},
	{
		CommandName: "docs.search_documents",
		Alias:       "search_documents",
		Category:    "Docs",
		Description: "Search documents by keyword across the workspace. Use only when you need to find other documents or the current document ID is unknown; do not use it to inspect a known current document.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
				"limit": map[string]any{"type": "integer", "description": "Maximum results to return (default 10, max 20)"},
			},
			"required": []string{"query"},
		},
	},
	{
		CommandName: "release.get_release_context",
		Alias:       "get_release_context",
		Category:    "Release",
		Description: "Load release metadata, compare commits/files against the previous published release, and resolve related tasks. Defaults the repository from the current repository target when available.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repository_id":         map[string]any{"type": "string", "description": "Optional repository ID. Defaults from the current repository target when omitted."},
				"repo_full_name":        map[string]any{"type": "string", "description": "Optional repository full name like owner/repo."},
				"tag_name":              map[string]any{"type": "string", "description": "The release tag name. Use the tag from the run trigger event when present."},
				"include_changed_files": map[string]any{"type": "boolean", "description": "Whether to include changed files from the release comparison."},
				"max_commits":           map[string]any{"type": "integer", "description": "Maximum number of commits to return. Default 100, max 200."},
				"max_files":             map[string]any{"type": "integer", "description": "Maximum number of changed files to return when include_changed_files is true. Default 200, max 500."},
			},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "release.find_tasks_for_git_changes",
		Alias:       "find_tasks_for_git_changes",
		Category:    "Release",
		Description: "Resolve tasks related to PRs, branches, commits, and text references for a repository. Returns evidence and confidence for each match.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repository_id":  map[string]any{"type": "string", "description": "Optional repository ID. Defaults from the current repository target when omitted."},
				"repo_full_name": map[string]any{"type": "string", "description": "Optional repository full name like owner/repo."},
				"pr_numbers":     map[string]any{"type": "array", "description": "Pull request numbers to resolve. Max 50.", "items": map[string]any{"type": "integer"}},
				"commit_shas":    map[string]any{"type": "array", "description": "Commit SHAs to resolve. Max 200.", "items": map[string]any{"type": "string"}},
				"branches":       map[string]any{"type": "array", "description": "Branch names to resolve. Max 50.", "items": map[string]any{"type": "string"}},
				"texts":          map[string]any{"type": "array", "description": "Free text to scan for task keys. Max 100.", "items": map[string]any{"type": "string"}},
			},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "release.get_task_context",
		Alias:       "get_task_context",
		Category:    "Release",
		Description: "Load compact task context with optional linked docs, document content, comments, and git links for specific task IDs.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_ids":                 map[string]any{"type": "array", "description": "Task IDs to load. Max 50.", "items": map[string]any{"type": "string"}},
				"include_linked_docs":      map[string]any{"type": "boolean", "description": "Whether to include linked document metadata."},
				"include_document_content": map[string]any{"type": "boolean", "description": "Whether to include linked document content text. Only used when include_linked_docs is true."},
				"include_comments":         map[string]any{"type": "boolean", "description": "Whether to include task comments."},
				"include_git_links":        map[string]any{"type": "boolean", "description": "Whether to include git links for each task."},
			},
			"required":             []string{"task_ids"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "docs.publish_prd_draft",
		Alias:       "publish_prd_draft",
		Category:    "Docs",
		Description: "Publish the current PRD markdown draft for epic planner review.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":   map[string]any{"type": "string", "description": "Optional preview title."},
				"content": map[string]any{"type": "string", "description": "Required. The full markdown PRD draft body under review."},
				"replace": map[string]any{"type": "boolean"},
			},
			"required":             []string{"content"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "docs.publish_task_plan_doc",
		Alias:       "publish_task_plan_doc",
		Category:    "Docs",
		Description: "Publish the current task planning document markdown for review. Always include the full markdown draft in \"content\"; do not send title-only payloads.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":   map[string]any{"type": "string", "description": "Optional preview title. The default is Task Planning Document."},
				"content": map[string]any{"type": "string", "description": "Required. The full markdown task planning document body under review, for example \"# Outcome\\n...\"."},
				"replace": map[string]any{"type": "boolean"},
			},
			"required":             []string{"content"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "docs.publish_document_change_proposal",
		Alias:       "publish_document_change_proposal",
		Category:    "Docs",
		Description: "Submit a proposed Docs document or block change for review in Docs. This persists a Docs proposal; after success, finish without calling request_approval.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"scope":       map[string]any{"type": "string", "enum": []string{"document", "block"}, "description": "Whether the proposal replaces the whole document or one addressable block."},
				"document_id": map[string]any{"type": "string", "description": "The document ID from the run context."},
				"block_id":    map[string]any{"type": "string", "description": "Required when scope is block. The stable block ID to replace."},
				"revision":    map[string]any{"type": "integer", "description": "Required when scope is block. The current block revision from get_document_blocks."},
				"content":     map[string]any{"type": "string", "description": "Replacement markdown. For document scope, provide the full document. For block scope, provide replacement markdown for the focused block only."},
				"summary":     map[string]any{"type": "string", "description": "Short human-readable summary of the proposed change."},
				"sources": map[string]any{
					"type":        "array",
					"description": "Optional source summaries used for the proposal.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"type":  map[string]any{"type": "string", "enum": []string{"conversation", "document", "url", "agent_run", "coverage_gap"}},
							"id":    map[string]any{"type": "string"},
							"label": map[string]any{"type": "string"},
							"url":   map[string]any{"type": "string"},
						},
						"required":             []string{"type", "label"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"scope", "document_id", "content", "summary"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "support.search_knowledge",
		Alias:       "search_knowledge",
		Category:    "Support",
		Description: "Search the workspace's support knowledge base (help docs, crawled content, curated guidance) with hybrid semantic search. Returns chunks with evidence_id values — cite these ids in send_support_reply claims. Chunks marked is_internal may inform your reasoning but must never be quoted or referenced to the visitor.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"queries": map[string]any{
					"type":        "array",
					"description": "1-3 search query variants (rephrase the visitor's question; add one variant with key product terms).",
					"items":       map[string]any{"type": "string"},
				},
				"language":    map[string]any{"type": "string", "description": "Optional ISO language code of the conversation."},
				"max_results": map[string]any{"type": "integer", "description": "Maximum chunks to return (default 12)."},
			},
			"required":             []string{"queries"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "support.send_reply",
		Alias:       "send_support_reply",
		Category:    "Support",
		Description: "Send your reply to the visitor. For factual answers you MUST first call search_knowledge and cite the evidence_id values that support each material claim — the server re-validates grounding and confidence, and hands the conversation to a human if validation fails. Call exactly once per visitor message, as your final action of the turn.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content":        map[string]any{"type": "string", "description": "The reply text shown to the visitor."},
				"reply_kind":     map[string]any{"type": "string", "enum": []string{"answer", "clarify", "conversational", "confirmation"}, "description": "answer = factual answer needing evidence; clarify = asking the visitor a question; conversational = greeting/small talk; confirmation = confirming the visitor's issue is resolved."},
				"confidence":     map[string]any{"type": "number", "description": "Your 0-1 confidence that the reply is correct and grounded."},
				"source_doc_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "evidence_id values (from search_knowledge) backing the reply."},
				"claims": map[string]any{
					"type":        "array",
					"description": "Each material factual claim in the reply mapped to the evidence ids that support it. Required for reply_kind=answer.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text":         map[string]any{"type": "string"},
							"evidence_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
						"required":             []string{"text", "evidence_ids"},
						"additionalProperties": false,
					},
				},
				"resolves_conversation": map[string]any{"type": "boolean", "description": "True only when the visitor confirmed their issue is resolved."},
			},
			"required":             []string{"content", "reply_kind", "confidence"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "support.escalate_to_human",
		Alias:       "escalate_to_human",
		Category:    "Support",
		Description: "Hand the conversation to a human teammate. Use when the visitor asks for a human, the request is risky (billing disputes, account deletion, legal), or you cannot answer from the knowledge base. The server sends the availability-aware handoff message — after calling this, end your turn without sending another reply.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"reason":        map[string]any{"type": "string", "description": "Short machine reason, e.g. customer_requested, out_of_scope, risky_request, cannot_answer."},
				"issue_key":     map[string]any{"type": "string", "description": "Optional stable key for the visitor's issue."},
				"issue_summary": map[string]any{"type": "string", "description": "Optional one-line summary for the teammate."},
			},
			"required":             []string{"reason"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "docs.list_spaces",
		Alias:       "list_spaces",
		Category:    "Docs",
		Description: "List the Docs spaces the current actor can see (id, name, slug, type, visibility). Use this to pick a space_id before create_document or move_document when the workspace has several spaces.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "docs.list_collections",
		Alias:       "list_collections",
		Category:    "Docs",
		Description: "List Docs collections (id, space_id, parent, name), optionally filtered to one space. Use to pick a collection_id when organizing documents.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"space_id": map[string]any{"type": "string", "description": "Optional space ID filter (from list_spaces)."},
			},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "agents.list_agents",
		Alias:       "list_agents",
		Category:    "Agents",
		Description: "List saved, built-in, and custom agents visible to the current actor (compact rows: id, name, preset, role, targets). Use query to search by name/preset and target_type to filter; reference agents by id.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":       map[string]any{"type": "string", "description": "Optional case-insensitive substring match on name, preset key, or role."},
				"target_type": map[string]any{"type": "string", "description": "Optional target type the agent must support (task, epic, document, crm_deal, repository, workspace, ...)."},
				"limit":       map[string]any{"type": "integer", "description": "Maximum rows to return (default 50)."},
			},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "agents.start_run",
		Alias:       "start_agent_run",
		Category:    "Agents",
		Description: "Start one bounded sub-agent run (a saved agent by id, or a Sub-agent with limited tools). Risk-based Dock agents pass the complete step directly; legacy approved launches may pass only approval_interaction_id. Support chat approval rules remain server-enforced. The result is delivered back into this chat when the run finishes.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id":                map[string]any{"type": "string", "description": "ID of the saved agent to run (from list_agents). Omit when use_command_agent is true."},
				"use_command_agent":       map[string]any{"type": "boolean", "description": "Run a Sub-agent instead of a saved agent. Requires allowed_tools."},
				"target":                  agentLaunchTargetSchema(),
				"instructions":            map[string]any{"type": "string", "description": "What the sub-agent run should do."},
				"allowed_tools":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Limited tool list for the sub-agent run (required for use_command_agent)."},
				"approval_interaction_id": map[string]any{"type": "string", "description": "Optional resolved legacy dock_plan_confirm or support_plan_confirm interaction ID."},
			},
			"required":             []string{"instructions"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "agents.start_plan",
		Alias:       "start_agent_plan",
		Category:    "Agents",
		Description: "Start a bounded multi-step plan of sub-agent runs (fan-out or dependency-ordered DAG via depends_on_step_indexes). Risk-based Dock agents pass the complete plan directly; legacy approved launches may pass only approval_interaction_id. Support chat approval rules remain server-enforced. Results are delivered back into this chat when the plan settles.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string", "description": "Short description of the overall plan."},
				"steps": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"agent_id":                map[string]any{"type": "string"},
							"use_command_agent":       map[string]any{"type": "boolean"},
							"target":                  agentLaunchTargetSchema(),
							"instructions":            map[string]any{"type": "string"},
							"allowed_tools":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"depends_on_step_indexes": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
						},
						"required":             []string{"instructions"},
						"additionalProperties": false,
					},
				},
				"approval_interaction_id": map[string]any{"type": "string", "description": "Optional resolved legacy dock_plan_confirm or support_plan_confirm interaction ID."},
			},
			"required":             []string{"steps"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "agents.get_run",
		Alias:       "get_agent_run",
		Category:    "Agents",
		Description: "Get the status of a sub-agent run or plan started from this chat. Use only when the user explicitly asks about progress — results arrive in this chat automatically.",
		Mutating:    false,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id":  map[string]any{"type": "string"},
				"plan_id": map[string]any{"type": "string"},
			},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "agents.cancel_run",
		Alias:       "cancel_agent_run",
		Category:    "Agents",
		Description: "Cancel a sub-agent run or plan started from this chat. No approval needed — cancelling stops work.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id":  map[string]any{"type": "string"},
				"plan_id": map[string]any{"type": "string"},
			},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "agents.create_agent",
		Alias:       "create_custom_agent",
		Category:    "Agents",
		Description: "Create a reusable custom agent from a description (drafted server-side). Requires a resolved dock_plan_confirm approval whose action matches this call exactly.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":                    map[string]any{"type": "string", "description": "Optional name override for the new agent."},
				"description":             map[string]any{"type": "string", "description": "What the agent should do."},
				"approval_interaction_id": map[string]any{"type": "string"},
			},
			"required":             []string{"description", "approval_interaction_id"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "agents.promote_run",
		Alias:       "promote_run_to_agent",
		Category:    "Agents",
		Description: "Promote a finished sub-agent run into a reusable saved agent. Requires a resolved dock_plan_confirm approval whose action matches this call exactly.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id":                  map[string]any{"type": "string"},
				"name":                    map[string]any{"type": "string"},
				"allowed_tools":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"allowed_targets":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"approval_interaction_id": map[string]any{"type": "string"},
			},
			"required":             []string{"run_id", "name", "approval_interaction_id"},
			"additionalProperties": false,
		},
	},
	{
		CommandName: "epic.run_delivery_pipeline",
		Alias:       "run_epic_delivery_pipeline",
		Category:    "Agents",
		Description: "Run the epic delivery pipeline: implement, review, and merge every open task of an epic on its integration branch, ordered by blocking links, then open the epic PR. From a dock chat this requires a dock_plan_confirm approval whose action is {\"epic_id\": ...}; epic-target runs may call it directly for their own epic.",
		Mutating:    true,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"epic_id":                 map[string]any{"type": "string", "description": "Epic to deliver. Defaults to the run's target when the run targets an epic."},
				"approval_interaction_id": map[string]any{"type": "string", "description": "Required when called from a dock chat."},
			},
			"additionalProperties": false,
		},
	},
}

func agentLaunchTargetSchema() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": "Target entity for the sub-agent run. Defaults to the workspace when omitted.",
		"properties": map[string]any{
			"type": map[string]any{"type": "string", "description": "Target entity type: workspace, task, epic, document, crm_deal, crm_contact, repository, support_conversation."},
			"id":   map[string]any{"type": "string", "description": "Target entity ID."},
		},
		"additionalProperties": false,
	}
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
