package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type fakeWorkspaceManager struct {
	req    CheckoutRepositoryRequest
	result *CheckoutRepositoryResult
	err    error
}

func (m *fakeWorkspaceManager) CheckoutRepository(_ context.Context, req CheckoutRepositoryRequest) (*CheckoutRepositoryResult, error) {
	m.req = req
	if m.err != nil {
		return nil, m.err
	}
	return m.result, nil
}

func TestCheckoutRepositoryUsesWorkspaceManagerAndRedactsOutput(t *testing.T) {
	registry := NewRegistry()
	manager := &fakeWorkspaceManager{
		result: &CheckoutRepositoryResult{
			Alias:        "api",
			Primary:      true,
			RepositoryID: "repo-1",
			RepoFullName: "owner/api",
			Lease: &agentcore.WorkspaceLease{
				ID:       "lease-1",
				Provider: "repository",
				RootPath: "/tmp/private-checkout",
				Metadata: map[string]interface{}{
					"repository_spec": map[string]interface{}{
						"auth": map[string]interface{}{"token": "secret-token"},
					},
				},
			},
		},
	}
	raw, err := registry.Execute(context.Background(), CallContext{WorkspaceManager: manager}, "checkout_repositories", json.RawMessage(`{"repositories":[{"repo_full_name":"owner/api","alias":"api"}]}`))
	if err != nil {
		t.Fatalf("checkout_repository: %v", err)
	}
	if manager.req.RepoFullName != "owner/api" || manager.req.Alias != "api" {
		t.Fatalf("unexpected manager request: %#v", manager.req)
	}
	text := string(raw)
	if strings.Contains(text, "secret-token") || strings.Contains(text, "private-checkout") || strings.Contains(text, "root_path") {
		t.Fatalf("checkout output leaked sensitive/internal data: %s", text)
	}
	if !strings.Contains(text, `"repo_full_name":"owner/api"`) || !strings.Contains(text, `"alias":"api"`) {
		t.Fatalf("checkout output missing repo identity: %s", text)
	}
}

func TestRepositoryCheckoutDefinitionsRequireListRepositoryIdentifiers(t *testing.T) {
	registry := NewRegistry()
	RegisterRepositoryCheckoutTools(registry)
	if _, ok := registry.Definition("checkout_repository"); ok {
		t.Fatal("legacy checkout_repository must not be registered")
	}

	for _, name := range []string{"checkout_repositories"} {
		definition, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		for _, required := range []string{"list_repositories", "repository_id", "repo_full_name", "not accepted"} {
			if !strings.Contains(definition.Description, required) {
				t.Errorf("%s description does not make repository identifiers explicit; missing %q in %q", name, required, definition.Description)
			}
		}
	}
}

func TestCheckoutRepositoriesRejectsNamesWithoutRepositoryIdentifier(t *testing.T) {
	registry := NewRegistry()
	RegisterRepositoryCheckoutTools(registry)
	manager := &fakeWorkspaceManager{result: &CheckoutRepositoryResult{Lease: &agentcore.WorkspaceLease{ID: "lease-1"}}}

	_, err := registry.Execute(context.Background(), CallContext{WorkspaceManager: manager}, "checkout_repositories", json.RawMessage(`{"repositories":[{"repository_id":"repo-1"},{"alias":"events-pipeline"}]}`))
	if err == nil || !strings.Contains(err.Error(), "repository_id or repo_full_name is required; call list_repositories and copy repository_id") {
		t.Fatalf("expected repair-oriented repository identifier error, got %v", err)
	}
}

func TestReadFilesCanSelectExtraRepository(t *testing.T) {
	registry := NewRegistry()
	primary := t.TempDir()
	extra := t.TempDir()
	if err := os.WriteFile(filepath.Join(primary, "README.md"), []byte("primary\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "README.md"), []byte("extra\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{
		ID:    "run-1",
		AppID: "app-a",
		WorkspaceLease: &agentcore.WorkspaceLease{
			ID:       "lease-primary",
			RootPath: primary,
			Metadata: map[string]interface{}{
				"repo_alias": "primary",
				"repository_workspaces": map[string]interface{}{
					"api": map[string]interface{}{
						"root_path":      extra,
						"alias":          "api",
						"repo_full_name": "owner/api",
					},
				},
			},
		},
	}
	raw, err := registry.Execute(context.Background(), CallContext{AppID: run.AppID, RunID: run.ID, Run: run}, "read_files", json.RawMessage(`{"files":[{"path":"README.md","repository":"api"}]}`))
	if err != nil {
		t.Fatalf("read_files: %v", err)
	}
	if got := workspaceToolString(t, raw); !strings.Contains(got, "extra") || strings.Contains(got, "primary") {
		t.Fatalf("expected extra repo content, got %q", got)
	}
}
