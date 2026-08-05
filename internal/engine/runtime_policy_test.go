package engine

import (
	"encoding/json"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/skills"
)

func TestRuntimePolicyFromExecutionConfig(t *testing.T) {
	policy, err := runtimePolicyFromExecutionConfig(json.RawMessage(`{
		"reasoning_effort":"high",
		"runtime_policy":{
			"completion_requires_interaction_kinds":["approval_request"],
			"interaction_contracts":[{"kind":"approval_request","schema":"approval_request_v1","transports":{"codex":{"type":"runtime_bridge"}}}]
		}
	}`))
	if err != nil {
		t.Fatalf("parse runtime policy: %v", err)
	}
	if len(policy.CompletionRequiresInteractionKinds) != 1 || policy.CompletionRequiresInteractionKinds[0] != skills.InteractionKindApprovalRequest {
		t.Fatalf("unexpected completion policy: %#v", policy)
	}
	contract, ok := policy.InteractionContract(skills.InteractionKindApprovalRequest)
	if !ok || contract.Schema != "approval_request_v1" || contract.Transports["codex"].Type != skills.TransportTypeRuntimeBridge {
		t.Fatalf("unexpected interaction contract: %#v", contract)
	}
}

func TestRuntimePolicyFromExecutionConfigLeavesLegacyConfigUnchanged(t *testing.T) {
	policy, err := runtimePolicyFromExecutionConfig(json.RawMessage(`{"reasoning_effort":"medium"}`))
	if err != nil {
		t.Fatalf("parse legacy execution config: %v", err)
	}
	if len(policy.CompletionRequiresInteractionKinds) != 0 || len(policy.InteractionContracts) != 0 || policy.AllowImplicitInvocation != nil {
		t.Fatalf("legacy config unexpectedly produced policy: %#v", policy)
	}
}
