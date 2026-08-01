package skills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestHTTPWorkspaceLookupPostsContextualRequest(t *testing.T) {
	var got LookupRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/active-by-key" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer skill-token" {
			t.Fatalf("unexpected auth header %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"skill": WorkspaceSkill{
			ID:           "skill-1",
			Key:          "workspace_skill",
			VersionKey:   "v1",
			Title:        "Workspace Skill",
			Description:  "workspace",
			SourceKind:   SourceWorkspace,
			Instructions: "Follow workspace guidance.",
			Policy: Policy{
				CompletionRequiresInteractionKinds: []string{InteractionKindApprovalRequest},
			},
		}})
	}))
	defer server.Close()

	lookup := HTTPWorkspaceLookup{BaseURL: server.URL, Token: "skill-token"}
	skill, err := lookup.GetActiveByKeyForContext(context.Background(), LookupRequest{
		LookupContext: LookupContext{
			AppID:   "host_app",
			AgentID: "agent-1",
			RunID:   "run-1",
			Target:  agentcore.TargetRef{Type: "task", ID: "task-1"},
			Metadata: map[string]interface{}{
				"workspace_id": "ws-1",
			},
		},
		Key: "workspace_skill",
	})
	if err != nil {
		t.Fatalf("lookup skill: %v", err)
	}
	if got.AppID != "host_app" || got.RunID != "run-1" || got.Target.ID != "task-1" || got.Metadata["workspace_id"] != "ws-1" || got.Key != "workspace_skill" {
		t.Fatalf("unexpected request %#v", got)
	}
	if skill == nil || skill.ID != "skill-1" || skill.Instructions != "Follow workspace guidance." {
		t.Fatalf("unexpected skill %#v", skill)
	}
	if len(skill.Policy.CompletionRequiresInteractionKinds) != 1 || skill.Policy.CompletionRequiresInteractionKinds[0] != InteractionKindApprovalRequest {
		t.Fatalf("expected lookup policy to preserve required approval, got %#v", skill.Policy)
	}
}

func TestHTTPWorkspaceLookupByIDAndNotFound(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			if r.URL.Path != "/by-id" {
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(WorkspaceSkill{ID: "skill-1", Key: "workspace_skill"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	lookup := HTTPWorkspaceLookup{BaseURL: server.URL}
	skill, err := lookup.GetByID(context.Background(), "app-a", "skill-1")
	if err != nil {
		t.Fatalf("lookup by id: %v", err)
	}
	if skill == nil || skill.ID != "skill-1" {
		t.Fatalf("unexpected skill %#v", skill)
	}
	missing, err := lookup.GetByID(context.Background(), "app-a", "missing")
	if err != nil {
		t.Fatalf("lookup missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("expected nil missing skill, got %#v", missing)
	}
}
