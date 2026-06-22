package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceToolRunCommandUsesWorkspaceDirectory(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	output, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"pwd"}`))
	if err != nil {
		t.Fatalf("run_command returned error: %v", err)
	}
	if strings.TrimSpace(workspaceToolString(t, output)) != callCtx.Run.WorkspaceLease.RootPath {
		t.Fatalf("expected pwd to run in workspace, got %q", workspaceToolString(t, output))
	}

	output, err = registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"cat","args":["sample.txt"]}`))
	if err != nil {
		t.Fatalf("run_command cat returned error: %v", err)
	}
	if workspaceToolString(t, output) != "hello" {
		t.Fatalf("unexpected cat output: %q", workspaceToolString(t, output))
	}
}

func TestWorkspaceToolRunCommandRejectsShellOperators(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	_, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"command":"echo ok && rm -rf tmp"}`))
	if err == nil || !strings.Contains(err.Error(), "shell operators are not allowed") {
		t.Fatalf("expected shell operator error, got %v", err)
	}
}

func TestWorkspaceToolRunCommandRejectsDisallowedProgram(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	_, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"sh","args":["-c","echo no"]}`))
	if err == nil || !strings.Contains(err.Error(), `command "sh" is not allowed`) {
		t.Fatalf("expected disallowed command error, got %v", err)
	}
}

func TestWorkspaceToolApplyPatchRequiresPriorReadForExistingFile(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt"), []byte("alpha\nbeta\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	patch := "*** Begin Patch\n*** Update File: sample.txt\n@@\n alpha\n-beta\n+beta-updated\n*** End Patch\n"
	_, err := registry.Execute(context.Background(), callCtx, "apply_patch", mustMarshalPatchInput(t, patch))
	if err == nil || !strings.Contains(err.Error(), "must read sample.txt before modifying it") {
		t.Fatalf("expected prior-read error, got %v", err)
	}
}

func TestWorkspaceToolApplyPatchUpdatesAndAddsFiles(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	existingPath := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(existingPath, []byte("alpha\nbeta\ngamma\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"sample.txt"}`)); err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}

	patch := "*** Begin Patch\n*** Update File: sample.txt\n@@\n alpha\n-beta\n+beta-updated\n gamma\n*** Add File: created.txt\n+new file\n+body\n*** End Patch\n"
	output, err := registry.Execute(context.Background(), callCtx, "apply_patch", mustMarshalPatchInput(t, patch))
	if err != nil {
		t.Fatalf("apply_patch returned error: %v", err)
	}
	if !strings.Contains(workspaceToolString(t, output), "Applied patch touching 2 file(s)") {
		t.Fatalf("unexpected output %q", workspaceToolString(t, output))
	}
	updatedData, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatalf("read updated file: %v", err)
	}
	if string(updatedData) != "alpha\nbeta-updated\ngamma\n" {
		t.Fatalf("unexpected updated content %q", string(updatedData))
	}
	createdData, err := os.ReadFile(filepath.Join(root, "created.txt"))
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(createdData) != "new file\nbody" {
		t.Fatalf("unexpected created content %q", string(createdData))
	}
}

func TestWorkspaceToolApplyPatchMovesFile(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	sourcePath := filepath.Join(root, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("alpha\nbeta\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_file", json.RawMessage(`{"path":"source.txt"}`)); err != nil {
		t.Fatalf("read_file returned error: %v", err)
	}

	patch := "*** Begin Patch\n*** Update File: source.txt\n*** Move to: moved.txt\n@@\n alpha\n-beta\n+beta-moved\n*** End Patch\n"
	if _, err := registry.Execute(context.Background(), callCtx, "apply_patch", mustMarshalPatchInput(t, patch)); err != nil {
		t.Fatalf("apply_patch returned error: %v", err)
	}
	if _, err := os.Stat(sourcePath); !os.IsNotExist(err) {
		t.Fatalf("expected source file to be removed, got err=%v", err)
	}
	movedData, err := os.ReadFile(filepath.Join(root, "moved.txt"))
	if err != nil {
		t.Fatalf("read moved file: %v", err)
	}
	if string(movedData) != "alpha\nbeta-moved\n" {
		t.Fatalf("unexpected moved content %q", string(movedData))
	}
}

func TestWorkspaceCommandPatchDefinitionsAreMutating(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"run_command", "apply_patch"} {
		def, ok := registry.Definition(name)
		if !ok {
			t.Fatalf("expected %s definition", name)
		}
		if !def.Mutating {
			t.Fatalf("expected %s to be mutating", name)
		}
	}
}

func mustMarshalPatchInput(t *testing.T, patch string) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"patch": patch})
	if err != nil {
		t.Fatalf("marshal patch input: %v", err)
	}
	return payload
}
