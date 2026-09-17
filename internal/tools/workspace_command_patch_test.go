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

func TestWorkspaceToolRunCommandAcceptsDoubleEncodedArgsArray(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "sample.txt"), []byte("double encoded"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	output, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"cat","args":"[\"sample.txt\"]"}`))
	if err != nil {
		t.Fatalf("run_command returned error: %v", err)
	}
	if workspaceToolString(t, output) != "double encoded" {
		t.Fatalf("unexpected cat output: %q", workspaceToolString(t, output))
	}
}

func TestWorkspaceToolRunCommandRejectsCommandTextInArgs(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	_, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"ls","args":"-la"}`))
	if err == nil || !strings.Contains(err.Error(), "args must be a string array") {
		t.Fatalf("expected structured args error, got %v", err)
	}
}

func TestWorkspaceToolRunCommandPersistsRepositoryBranchChanges(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	callCtx.Run.WorkspaceLease.Provider = "repository"
	if output, err := runWorkspaceGit(context.Background(), root, "init", "--initial-branch", "main"); err != nil {
		t.Fatalf("initialize repository: %v: %s", err, output)
	}
	manager := &recordingBranchWorkspaceManager{}
	callCtx.WorkspaceManager = manager

	_, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"git","args":["checkout","-b","feature/from-command"]}`))
	if err != nil {
		t.Fatalf("run_command returned error: %v", err)
	}
	if manager.branch != "feature/from-command" {
		t.Fatalf("persisted branch = %q, want feature/from-command", manager.branch)
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

func TestWorkspaceToolRunCommandAllowsPackagedDeveloperToolchain(t *testing.T) {
	for _, program := range []string{
		"go", "make", "node", "npm", "npx", "pnpm", "yarn",
		"python", "python3", "pip", "pip3", "pytest", "uv", "poetry",
		"rustc", "cargo", "git", "rg",
	} {
		if !defaultAllowedCommands[program] {
			t.Errorf("expected packaged developer command %q to be allowed", program)
		}
	}
}

func TestWorkspaceToolRunCommandEnforcesReadOnlyPolicy(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	callCtx.Agent = &agentcore.Agent{ExecutionConfig: json.RawMessage(`{"workspace":{"access":"read_only"}}`)}

	if _, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"cat","args":["missing.txt"]}`)); err == nil || !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("expected permitted cat to report its missing-file error, got %v", err)
	}
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"program":"python","args":["-c","open('changed.txt','w').write('x')"]}`),
		json.RawMessage(`{"program":"rm","args":["-f","sample.txt"]}`),
		json.RawMessage(`{"program":"git","args":["checkout","-b","changed"]}`),
		json.RawMessage(`{"program":"git","args":["diff","--output=changed.diff"]}`),
	} {
		if _, err := registry.Execute(context.Background(), callCtx, "run_command", input); err == nil || !strings.Contains(err.Error(), "read-only workspace") {
			t.Fatalf("expected read-only policy rejection for %s, got %v", input, err)
		}
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

func TestRunCommandWorkingDirectory(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	root := callCtx.Run.WorkspaceLease.RootPath
	if err := os.Mkdir(filepath.Join(root, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "sample"), []byte("nested contents"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	output, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"cat","args":["sample"],"working_directory":"nested"}`))
	if err != nil || workspaceToolString(t, output) != "nested contents" {
		t.Fatalf("nested command: %s %v", output, err)
	}
	for _, directory := range []string{"..", "/tmp", "outside", "missing", "nested/sample"} {
		input, _ := json.Marshal(map[string]any{"program": "pwd", "working_directory": directory})
		if _, err := registry.Execute(context.Background(), callCtx, "run_command", input); err == nil {
			t.Errorf("accepted directory %q", directory)
		}
	}
	if _, err := registry.Execute(context.Background(), callCtx, "run_command", json.RawMessage(`{"program":"pwd","cwd":"nested"}`)); err == nil {
		t.Error("silently ignored unknown working directory field")
	}
}

func TestPreviewRunRejectsPublishingToolsAndCommands(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	callCtx.Run.Input.Metadata = map[string]interface{}{"delivery_mode": "preview"}
	for _, name := range []string{"commit_and_push", "open_pr"} {
		if _, err := registry.Execute(context.Background(), callCtx, name, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "preview") {
			t.Fatalf("tool=%s err=%v", name, err)
		}
	}
	for _, args := range [][]string{{"push"}, {"commit", "--allow-empty", "-m", "test"}, {"-c", "alias.publish=push", "publish"}} {
		input, _ := json.Marshal(map[string]interface{}{"program": "git", "args": args})
		if _, err := registry.Execute(context.Background(), callCtx, "run_command", input); err == nil || !strings.Contains(err.Error(), "preview") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}
