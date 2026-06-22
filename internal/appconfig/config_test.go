package appconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestApplyRegistersHTTPContextAdapter(t *testing.T) {
	var got host.TargetContextRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ctx-token" {
			t.Fatalf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(host.TargetContext{
			Target:  got.Target,
			Summary: "configured context",
		})
	}))
	defer server.Close()

	adapters := host.NewAdapterRegistry(host.NewStaticContextProvider())
	registry := tools.NewRegistry()
	workspaces := workspace.NewRegistry()
	err := Apply(context.Background(), &Config{Apps: []App{{
		AppID:           "app-a",
		ContextEndpoint: server.URL,
		ContextToken:    "ctx-token",
	}}}, adapters, registry, workspaces)
	if err != nil {
		t.Fatalf("apply config: %v", err)
	}
	resolved, err := adapters.ResolveRunTarget(context.Background(), host.TargetContextRequest{
		AppID: "app-a",
		RunID: "run-1",
		Target: agentcore.TargetRef{
			Type: "ticket",
			ID:   "T-1",
		},
	})
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	if got.RunID != "run-1" || resolved.Summary != "configured context" {
		t.Fatalf("unexpected context request/response: got=%#v resolved=%#v", got, resolved)
	}
}

func TestApplyRegistersHTTPWorkspaceProvider(t *testing.T) {
	var got workspace.PrepareRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prepare" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer workspace-token" {
			t.Fatalf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(agentcore.WorkspaceLease{
			ID:       "lease-1",
			Provider: "test",
			RootPath: "/tmp/repo",
		})
	}))
	defer server.Close()

	adapters := host.NewAdapterRegistry(host.NewStaticContextProvider())
	registry := tools.NewRegistry()
	workspaces := workspace.NewRegistry()
	err := Apply(context.Background(), &Config{Apps: []App{{
		AppID: "app-a",
		WorkspaceProvider: &WorkspaceProvider{
			Transport: "http",
			BaseURL:   server.URL,
			Token:     "workspace-token",
		},
	}}}, adapters, registry, workspaces)
	if err != nil {
		t.Fatalf("apply config: %v", err)
	}
	provider, ok := workspaces.Provider("app-a")
	if !ok {
		t.Fatal("expected workspace provider")
	}
	lease, err := provider.PrepareWorkspace(context.Background(), workspace.PrepareRequest{
		AppID: "app-a",
		RunID: "run-1",
		Target: agentcore.TargetRef{
			Type: "repository",
			ID:   "repo-1",
		},
	})
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if got.RunID != "run-1" || lease.ID != "lease-1" || lease.CleanupPolicy != workspace.CleanupOnTerminal {
		t.Fatalf("unexpected workspace request/response: got=%#v lease=%#v", got, lease)
	}
}

func TestApplyRegistersHTTPCommandProvider(t *testing.T) {
	var got tools.CommandExecutionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/execute" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer command-token" {
			t.Fatalf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(tools.CommandExecutionResponse{Output: json.RawMessage(`{"updated":true}`)})
	}))
	defer server.Close()

	adapters := host.NewAdapterRegistry(host.NewStaticContextProvider())
	registry := tools.NewRegistry()
	workspaces := workspace.NewRegistry()
	err := Apply(context.Background(), &Config{Apps: []App{{
		AppID: "helpin",
		CommandProvider: &CommandProvider{
			Transport: "http",
			BaseURL:   server.URL,
			Token:     "command-token",
		},
	}}}, adapters, registry, workspaces)
	if err != nil {
		t.Fatalf("apply config: %v", err)
	}
	def, ok := registry.Definition("update_task_state")
	if !ok || !def.Mutating {
		t.Fatalf("expected command-backed update_task_state definition, got %#v", def)
	}
	run := &agentcore.AgentRun{
		ID:      "run-1",
		AppID:   "helpin",
		AgentID: "agent-1",
		Target:  agentcore.TargetRef{Type: "task", ID: "task-1"},
		Input:   agentcore.RunInput{Metadata: map[string]interface{}{"workspace_id": "ws-1"}},
	}
	output, err := registry.Execute(context.Background(), tools.CallContext{AppID: "helpin", RunID: "run-1", Run: run}, "update_task_state", json.RawMessage(`{"state_id":"done"}`))
	if err != nil {
		t.Fatalf("execute command tool: %v", err)
	}
	if string(output) != `{"updated":true}` {
		t.Fatalf("unexpected output: %s", output)
	}
	if got.CommandName != "pm.update_task_state" || got.Meta.WorkspaceID != "ws-1" || got.Meta.TargetType != "task" || got.Meta.TargetID != "task-1" {
		t.Fatalf("unexpected command request: %#v", got)
	}
}

func TestApplySkillLookupsRegistersHTTPSkillProvider(t *testing.T) {
	var got skills.LookupRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/active-by-key" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer skill-token" {
			t.Fatalf("unexpected auth header: %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(skills.WorkspaceSkill{
			ID:           "skill-1",
			Key:          "workspace_skill",
			VersionKey:   "v1",
			Title:        "Workspace Skill",
			Description:  "workspace",
			SourceKind:   skills.SourceWorkspace,
			Instructions: "Use workspace instructions.",
		})
	}))
	defer server.Close()

	registry := skills.NewRegistry()
	packageStores := skills.NewPackageStoreRegistry()
	err := ApplySkillProviders(context.Background(), &Config{Apps: []App{{
		AppID: "app-a",
		SkillProvider: &SkillProvider{
			Transport:      "http",
			BaseURL:        server.URL,
			Token:          "skill-token",
			PackageBaseURL: server.URL,
		},
	}}}, registry, packageStores)
	if err != nil {
		t.Fatalf("apply skill lookups: %v", err)
	}
	resolution, err := registry.ResolveForContext(context.Background(), skills.LookupContext{
		AppID: "app-a",
		RunID: "run-1",
		Target: agentcore.TargetRef{
			Type: "task",
			ID:   "task-1",
		},
	}, []agentcore.SkillRef{{Key: "workspace_skill"}})
	if err != nil {
		t.Fatalf("resolve workspace skill: %v", err)
	}
	if got.RunID != "run-1" || got.Target.ID != "task-1" {
		t.Fatalf("unexpected lookup request: %#v", got)
	}
	if len(resolution.CoreRefs) != 1 || resolution.CoreRefs[0].SkillID != "skill-1" {
		t.Fatalf("unexpected resolution: %#v", resolution)
	}
	if _, ok := packageStores.Store("app-a"); !ok {
		t.Fatal("expected skill package store")
	}
}
