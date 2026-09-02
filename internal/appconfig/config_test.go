package appconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

func TestApplyRequiredProviderUsesBoundedStartupRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	registry := tools.NewRegistry()
	started := time.Now()
	err := ApplyWithOptions(context.Background(), &Config{Apps: []App{{
		AppID:        "usermaven",
		MCPProviders: []MCPProvider{{Name: "usermaven", Transport: "http", URL: server.URL}},
	}}}, host.NewAdapterRegistry(host.NewStaticContextProvider()), registry, workspace.NewRegistry(), ApplyOptions{RequiredProviderStartupTimeout: 25 * time.Millisecond})
	if err == nil {
		t.Fatal("expected required provider startup to fail")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded startup retry took %s", elapsed)
	}
	if registry.Ready() {
		t.Fatal("unavailable required provider reported ready")
	}
}

func TestApplyWorkerDefersPollingForUnavailableRequiredProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := tools.NewRegistry()
	err := ApplyWithOptions(ctx, &Config{Apps: []App{{
		AppID:        "usermaven",
		MCPProviders: []MCPProvider{{Name: "usermaven", Transport: "http", URL: server.URL}},
	}}}, host.NewAdapterRegistry(host.NewStaticContextProvider()), registry, workspace.NewRegistry(), ApplyOptions{ContinueOnRequiredProviderFailure: true})
	if err != nil {
		t.Fatalf("worker apply: %v", err)
	}
	if registry.Ready() {
		t.Fatal("worker should remain not ready until required provider recovers")
	}
}

func TestApplyRegistersUnprefixedMCPProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tools" {
			t.Fatalf("unexpected provider path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"tools": []map[string]interface{}{{
			"name": "create_collection", "description": "Create a collection.",
			"input_schema": map[string]interface{}{"type": "object"}, "mutating": true,
		}}})
	}))
	defer server.Close()
	adapters := host.NewAdapterRegistry(host.NewStaticContextProvider())
	registry := tools.NewRegistry()
	err := Apply(context.Background(), &Config{Apps: []App{{
		AppID:        "helpin",
		MCPProviders: []MCPProvider{{Name: "helpin", Transport: "http", URL: server.URL, ToolNamespace: "none", StartupPolicy: "required"}},
	}}}, adapters, registry, workspace.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := registry.DefinitionForApp("helpin", "create_collection")
	if !ok || definition.Description != "Create a collection." {
		t.Fatalf("unprefixed provider was not registered: %#v", definition)
	}
	if !registry.Ready() || len(registry.ProviderHealth()) != 1 || registry.ProviderHealth()[0].Source != "mcp" {
		t.Fatalf("unexpected provider health: %#v", registry.ProviderHealth())
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

func TestApplyRegistersBrowserToolsOnlyForConfiguredApp(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_BROWSER_ENABLED", "true")
	t.Setenv("KERNEL_API_KEY", "kernel-key")
	adapters := host.NewAdapterRegistry(host.NewStaticContextProvider())
	registry := tools.NewRegistry()
	workspaces := workspace.NewRegistry()
	err := Apply(context.Background(), &Config{Apps: []App{
		{AppID: "app-a", Browser: &BrowserConfig{Enabled: true, AllowedDomains: []string{"*"}, ArtifactProvider: &ArtifactProvider{UploadEndpoint: "https://app-a.test/artifacts", Token: "app-a-token"}}},
		{AppID: "app-b"},
	}}, adapters, registry, workspaces)
	if err != nil {
		t.Fatalf("apply config: %v", err)
	}
	if _, ok := registry.DefinitionForApp("app-a", "browser_open"); !ok {
		t.Fatal("configured app is missing browser_open")
	}
	if def, ok := registry.DefinitionForApp("app-a", "browser_screenshot"); !ok || def.RiskLevel != tools.RiskLevelRoutine {
		t.Fatalf("configured app is missing routine browser_screenshot: %#v", def)
	}
	if def, ok := registry.DefinitionForApp("app-a", "browser_record"); !ok || !def.Mutating || def.RiskLevel != tools.RiskLevelRoutine {
		t.Fatalf("configured app is missing routine browser_record: %#v", def)
	}
	if _, ok := registry.DefinitionForApp("app-b", "browser_open"); ok {
		t.Fatal("unconfigured app unexpectedly received browser tools")
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
