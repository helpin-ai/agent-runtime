package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceEditsPreserveTextFormat(t *testing.T) {
	for _, tool := range []string{"edit_file", "apply_patch"} {
		for _, tt := range []struct {
			name, before, old, replacement, after string
			wantError                             bool
		}{
			{"LF", "first\nmiddle  \nlast\n", "first", "updated\ninserted", "updated\ninserted\nmiddle  \nlast\n", false},
			{"CRLF", "first\r\nmiddle  \r\nlast\r\n", "first", "updated\ninserted", "updated\r\ninserted\r\nmiddle  \r\nlast\r\n", false},
			{"BOM at first line", "\ufefffirst\nmiddle\nlast\n", "first", "updated", "\ufeffupdated\nmiddle\nlast\n", false},
			{"CRLF and BOM", "\ufefffirst\r\nmiddle\r\nlast\r\n", "first\nmiddle", "updated\ninserted", "\ufeffupdated\r\ninserted\r\nlast\r\n", false},
			{"no final newline", "first\nmiddle\nlast", "last", "updated", "first\nmiddle\nupdated", false},
			{"CRLF no final newline", "\ufefffirst\r\nmiddle\r\nlast", "last", "updated", "\ufefffirst\r\nmiddle\r\nupdated", false},
			{"mixed endings exact LF match", "first\r\nmiddle\nlast\r\n", "middle", "updated\ninserted", "first\r\nupdated\ninserted\nlast\r\n", false},
			{"mixed endings with BOM", "\ufefffirst\r\nmiddle\nlast\r\n", "middle", "updated", "\ufefffirst\r\nupdated\nlast\r\n", false},
			{"mixed endings must not normalize", "first\r\nmiddle\nlast\r\n", "first\nmiddle", "updated", "first\r\nmiddle\nlast\r\n", true},
		} {
			t.Run(tool+"/"+tt.name, func(t *testing.T) {
				registry, callCtx := workspaceToolTestRegistry(t)
				path := writeWorkspaceFixture(t, callCtx, "sample.txt", tt.before)
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
				if _, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"sample.txt"}]}`)); err != nil {
					t.Fatal(err)
				}
				input, err := json.Marshal(map[string]string{"path": "sample.txt", "old_string": tt.old, "new_string": tt.replacement})
				if err != nil {
					t.Fatal(err)
				}
				if tool == "apply_patch" {
					patch := "*** Begin Patch\n*** Update File: sample.txt\n@@\n-" + strings.ReplaceAll(tt.old, "\n", "\n-") + "\n+" + strings.ReplaceAll(tt.replacement, "\n", "\n+") + "\n*** End Patch\n"
					input = mustMarshalPatchInput(t, patch)
				}
				_, err = registry.Execute(context.Background(), callCtx, tool, input)
				if tt.wantError {
					if err == nil || !strings.Contains(err.Error(), "did not match") {
						t.Fatalf("expected exact-match rejection, got %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(path)
				if err != nil || string(got) != tt.after {
					t.Fatalf("file bytes = %q, want %q, err = %v", got, tt.after, err)
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0755 {
					t.Fatalf("edit changed file permissions: %v", err)
				}
			})
		}
	}
}

func TestApplyPatchLaterHunkFailureDoesNotWritePlan(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	files := map[string]string{"a.txt": "\ufeffone\r\n", "b.txt": "two\n", "delete.txt": "keep\n"}
	for path, content := range files {
		writeWorkspaceFixture(t, callCtx, path, content)
	}
	if _, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"a.txt"},{"path":"b.txt"},{"path":"delete.txt"}]}`)); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Add File: added.txt\n+new\n*** Delete File: delete.txt\n*** Update File: a.txt\n@@\n-one\n+ONE\n*** Update File: b.txt\n@@\n-two\n+TWO\n@@\n-missing\n+changed\n*** End Patch\n"
	if _, err := registry.Execute(context.Background(), callCtx, "apply_patch", mustMarshalPatchInput(t, patch)); err == nil || !strings.Contains(err.Error(), "hunk did not match") {
		t.Fatalf("expected later-hunk mismatch: %v", err)
	}
	for path, content := range files {
		got, err := os.ReadFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, path))
		if err != nil || string(got) != content {
			t.Fatalf("failed patch changed %s: %q, %v", path, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "added.txt")); !os.IsNotExist(err) {
		t.Fatalf("failed patch created a file: %v", err)
	}
}
