package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

type recoveryRepositorySpec struct{ cloneURL string }

func (s recoveryRepositorySpec) ResolveRepositoryWorkspace(context.Context, workspace.PrepareRequest) (*workspace.RepositoryWorkspaceSpec, error) {
	return &workspace.RepositoryWorkspaceSpec{CloneURL: s.cloneURL, BaseBranch: "main"}, nil
}

func TestRepositoryContinuationAcrossWorkers(t *testing.T) {
	remote := t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = remote
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for _, tt := range []struct {
		name              string
		allowed, narrowed []string
		invalidCheckout   bool
		wantRecovery      bool
		ephemeral         bool
		keepOld           bool
	}{
		{name: "read-only missing", allowed: []string{"read_files"}, wantRecovery: true},
		{name: "read-only invalid", allowed: []string{"read_files"}, invalidCheckout: true, wantRecovery: true},
		{name: "narrowed to reads", allowed: []string{"read_files", "apply_patch"}, narrowed: []string{"read_files"}, wantRecovery: true},
		{name: "shell missing", allowed: []string{"run_command"}},
		{name: "write missing", allowed: []string{"edit_file"}},
		{name: "write invalid", allowed: []string{"apply_patch"}, invalidCheckout: true},
		{name: "ephemeral shell missing", allowed: []string{"run_command"}, ephemeral: true, wantRecovery: true},
		{name: "ephemeral write invalid", allowed: []string{"apply_patch"}, invalidCheckout: true, ephemeral: true, wantRecovery: true},
		{name: "new session on same pod rejects stale checkout", allowed: []string{"run_command"}, ephemeral: true, keepOld: true, wantRecovery: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.ephemeral {
				ctx = workspace.WithSession(ctx, "first-worker-session")
			}
			mem := store.NewMemory()
			agent := testAgent("app-a")
			agent.AllowedTools = tt.allowed
			// The effective tools, not the workspace label, determine whether recovery is safe.
			agent.ExecutionConfig = []byte(`{"workspace":{"mode":"repository","access":"read_only"}}`)
			run := &agentcore.AgentRun{AppID: agent.AppID, RuntimeKind: agent.RuntimeKind, Input: agentcore.RunInput{AllowedTools: tt.narrowed}}
			if err := mem.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			registry := workspace.NewRegistry()
			provider := workspace.RepositoryProvider{RootDir: t.TempDir(), SpecProvider: recoveryRepositorySpec{remote}}
			if err := registry.Register(agent.AppID, provider); err != nil {
				t.Fatal(err)
			}
			eng := New(Config{Store: mem, Workspaces: registry})
			lease, err := eng.ensureWorkspace(ctx, &agent, run, nil)
			if err != nil {
				t.Fatal(err)
			}
			oldRoot := lease.RootPath
			lostPath := oldRoot
			if tt.invalidCheckout {
				lostPath = filepath.Join(oldRoot, ".git")
			}
			if !tt.keepOld {
				if err := os.RemoveAll(lostPath); err != nil {
					t.Fatal(err)
				}
				provider.RootDir = t.TempDir() // A second worker has a different local filesystem.
			}
			if tt.ephemeral {
				ctx = workspace.WithSession(ctx, "second-worker-session")
			}
			if err := registry.Register(agent.AppID, provider); err != nil {
				t.Fatal(err)
			}
			lease, err = eng.ensureWorkspace(ctx, &agent, run, nil)
			if !tt.wantRecovery {
				if err == nil || !strings.Contains(err.Error(), "workspace for this run is unavailable") {
					t.Fatalf("expected unavailable coding workspace, got %v", err)
				}
				entries, err := os.ReadDir(provider.RootDir)
				if err != nil || len(entries) != 0 || run.WorkspaceLease.RootPath != oldRoot {
					t.Fatal("coding continuation must not create a replacement checkout or change its lease")
				}
				return
			}
			if err != nil {
				t.Fatalf("recover read-only checkout: %v", err)
			}
			if lease.RootPath == oldRoot || !strings.HasPrefix(lease.RootPath, provider.RootDir+string(os.PathSeparator)) {
				t.Fatalf("checkout was not prepared on the new worker: %s", lease.RootPath)
			}
			if _, err := os.Stat(filepath.Join(lease.RootPath, ".git")); err != nil {
				t.Fatalf("replacement is not a Git checkout: %v", err)
			}
			stored, err := mem.GetRun(ctx, run.AppID, run.ID)
			if err != nil || stored.WorkspaceLease.RootPath != lease.RootPath {
				t.Fatalf("replacement lease was not persisted: %v", err)
			}
			if tt.ephemeral {
				if stored.Input.Metadata[workspace.RecoveryMetadataKey] == nil || lease.Metadata[workspace.SessionMetadataKey] != "second-worker-session" {
					t.Fatal("fresh checkout must persist recovery notice and session ownership")
				}
				marker := stored.Input.Metadata[workspace.RecoveryMetadataKey]
				again, err := eng.ensureWorkspace(ctx, &agent, run, nil)
				if err != nil || again.RootPath != lease.RootPath || run.Input.Metadata[workspace.RecoveryMetadataKey] != marker {
					t.Fatalf("healthy session should reuse its checkout: %v", err)
				}
			}
		})
	}
}
