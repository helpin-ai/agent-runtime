package workspace

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionRootsAndScratchRecovery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", root)
	one, two := WithSession(context.Background(), "../../one"), WithSession(context.Background(), "two")
	a, err := NewScratchInSession(one, "app", "run")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewScratchInSession(two, "app", "run")
	if err != nil {
		t.Fatal(err)
	}
	if a.RootPath == b.RootPath || !strings.HasPrefix(a.RootPath, filepath.Join(root, ".sessions")+string(filepath.Separator)) || LeaseInSession(two, a) {
		t.Fatal("session isolation failed")
	}
	if !LeaseInSession(one, a) {
		t.Fatal("healthy scratch rejected")
	}
}

func TestWorkspaceStorageValidation(t *testing.T) {
	t.Setenv(StorageModeEnv, "ephemerl")
	if err := ValidateWorkspaceStorage(); err == nil {
		t.Fatal("invalid storage mode accepted")
	}
	t.Setenv(StorageModeEnv, "ephemeral")
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", filepath.Join(t.TempDir(), "missing"))
	if err := ValidateWorkspaceStorage(); err == nil {
		t.Fatal("missing local root accepted")
	}
	t.Setenv("AGENT_RUNTIME_WORKSPACE_ROOT", t.TempDir())
	if err := ValidateWorkspaceStorage(); err != nil {
		t.Fatal(err)
	}
}
