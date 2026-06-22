package skills

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestRegistryResolveAggregatesPolicyAndCanonicalRefs(t *testing.T) {
	allow := true
	registry := NewRegistry(
		Definition{
			Key:               "approval_protocol",
			Title:             "Approval",
			Description:       "approval",
			SourceKind:        SourceBuiltIn,
			Instructions:      "Use approval.",
			RequiredTools:     []string{"request_approval"},
			SupportedRuntimes: []string{agentcore.RuntimeNativeSDK},
			Policy: Policy{
				AllowImplicitInvocation:            &allow,
				CompletionRequiresInteractionKinds: []string{InteractionKindApprovalRequest},
				InteractionContracts: []InteractionContract{{
					Kind:   InteractionKindApprovalRequest,
					Schema: "approval_request_v1",
					Transports: map[string]InteractionTransport{
						agentcore.RuntimeNativeSDK: {Type: TransportTypeToolCall, ToolName: "request_approval"},
					},
				}},
			},
		},
		Definition{
			Key:          "input_protocol",
			Title:        "Input",
			Description:  "input",
			SourceKind:   SourceBuiltIn,
			Instructions: "Ask focused questions.",
			Policy: Policy{
				CompletionRequiresInteractionKinds: []string{InteractionKindRequestUserInput, InteractionKindApprovalRequest},
				InteractionContracts: []InteractionContract{{
					Kind: InteractionKindApprovalRequest,
					Transports: map[string]InteractionTransport{
						"codex": {Type: TransportTypeRuntimeBridge},
					},
				}},
			},
		},
	)

	resolution, err := registry.Resolve(context.Background(), "app-a", []agentcore.SkillRef{
		{Key: "approval_protocol"},
		{Key: "input_protocol"},
	})
	if err != nil {
		t.Fatalf("resolve skills: %v", err)
	}
	if len(resolution.Refs) != 2 || resolution.CoreRefs[0].Key != "approval_protocol" {
		t.Fatalf("unexpected refs: %#v", resolution)
	}
	if resolution.Instructions != "Use approval.\n\nAsk focused questions." {
		t.Fatalf("unexpected instructions: %q", resolution.Instructions)
	}
	if got := resolution.Policy.CompletionRequiresInteractionKinds; len(got) != 2 || got[0] != InteractionKindApprovalRequest || got[1] != InteractionKindRequestUserInput {
		t.Fatalf("unexpected required interactions: %#v", got)
	}
	contract, ok := resolution.Policy.InteractionContract(InteractionKindApprovalRequest)
	if !ok {
		t.Fatal("expected approval contract")
	}
	if contract.Schema != "approval_request_v1" || contract.Transports[agentcore.RuntimeNativeSDK].ToolName != "request_approval" || contract.Transports["codex"].Type != TransportTypeRuntimeBridge {
		t.Fatalf("unexpected merged contract: %#v", contract)
	}
}

func TestRegistryResolveRejectsDuplicates(t *testing.T) {
	registry := NewRegistry(Definition{Key: "a", Description: "a", Instructions: "A"})
	_, err := registry.Resolve(context.Background(), "app-a", []agentcore.SkillRef{{Key: "a"}, {Key: "a"}})
	if err == nil {
		t.Fatal("expected duplicate skill reference error")
	}
}

func TestRegistryResolveForContextUsesContextualLookup(t *testing.T) {
	lookup := &recordingContextualLookup{skill: &WorkspaceSkill{
		ID:                "skill-1",
		Key:               "workspace_skill",
		VersionKey:        "v1",
		Title:             "Workspace Skill",
		Description:       "workspace",
		SourceKind:        SourceWorkspace,
		Instructions:      "Use workspace instructions.",
		SupportedRuntimes: []string{agentcore.RuntimeNativeSDK},
	}}
	registry := NewRegistry()
	registry.SetWorkspaceLookupForApp("app-a", lookup)

	resolution, err := registry.ResolveForContext(context.Background(), LookupContext{
		AppID:   "app-a",
		AgentID: "agent-1",
		RunID:   "run-1",
		Target:  agentcore.TargetRef{Type: "task", ID: "task-1"},
		Metadata: map[string]interface{}{
			"workspace_id": "ws-1",
		},
	}, []agentcore.SkillRef{{Key: "workspace_skill"}})
	if err != nil {
		t.Fatalf("resolve workspace skill: %v", err)
	}
	if lookup.activeByKeyReq.RunID != "run-1" || lookup.activeByKeyReq.Target.ID != "task-1" || lookup.activeByKeyReq.Metadata["workspace_id"] != "ws-1" {
		t.Fatalf("context was not passed to lookup: %#v", lookup.activeByKeyReq)
	}
	if len(resolution.CoreRefs) != 1 || resolution.CoreRefs[0].SkillID != "skill-1" || resolution.CoreRefs[0].VersionKey != "v1" {
		t.Fatalf("unexpected canonical refs: %#v", resolution.CoreRefs)
	}
	if resolution.Instructions != "Use workspace instructions." {
		t.Fatalf("unexpected instructions %q", resolution.Instructions)
	}
}

func TestValidateRuntimeAndToolsAllowsCodexNativeMigration(t *testing.T) {
	definitions := []Definition{{
		Key:               "native_skill",
		RequiredTools:     []string{"request_human_input"},
		SupportedRuntimes: []string{agentcore.RuntimeNativeSDK},
	}}
	if err := ValidateRuntimeAndTools(agentcore.RuntimeCodex, []string{"request_user_input"}, definitions); err != nil {
		t.Fatalf("expected codex to accept native-compatible skill: %v", err)
	}
	if err := ValidateRuntimeAndTools(agentcore.RuntimeNativeSDK, []string{"get_context"}, definitions); err == nil {
		t.Fatal("expected missing required tool error")
	}
}

func TestRequestUserInputUsesRuntimeBridgeDefault(t *testing.T) {
	if !RequestUserInputUsesRuntimeBridge(Policy{}, "codex") {
		t.Fatal("expected codex to default to runtime bridge")
	}
	if RequestUserInputUsesRuntimeBridge(Policy{}, agentcore.RuntimeNativeSDK) {
		t.Fatal("native should not default to runtime bridge")
	}
}

type recordingContextualLookup struct {
	skill          *WorkspaceSkill
	byIDReq        LookupRequest
	activeByKeyReq LookupRequest
}

func (l *recordingContextualLookup) GetByID(ctx context.Context, appID, id string) (*WorkspaceSkill, error) {
	return l.GetByIDForContext(ctx, LookupRequest{LookupContext: LookupContext{AppID: appID}, SkillID: id})
}

func (l *recordingContextualLookup) GetActiveByKey(ctx context.Context, appID, key string) (*WorkspaceSkill, error) {
	return l.GetActiveByKeyForContext(ctx, LookupRequest{LookupContext: LookupContext{AppID: appID}, Key: key})
}

func (l *recordingContextualLookup) GetByIDForContext(_ context.Context, req LookupRequest) (*WorkspaceSkill, error) {
	l.byIDReq = cloneLookupRequest(req)
	return l.skill, nil
}

func (l *recordingContextualLookup) GetActiveByKeyForContext(_ context.Context, req LookupRequest) (*WorkspaceSkill, error) {
	l.activeByKeyReq = cloneLookupRequest(req)
	return l.skill, nil
}

func cloneLookupRequest(req LookupRequest) LookupRequest {
	payload, _ := json.Marshal(req)
	var clone LookupRequest
	_ = json.Unmarshal(payload, &clone)
	return clone
}
