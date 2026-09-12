package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceMutationRejectsMissingFieldsWithoutWriting(t *testing.T) {
	for _, tt := range []struct {
		tool, field, input string
	}{
		{"write_file", "content", `{"path":"existing.txt"}`},
		{"write_file", "content", `{"path":"existing.txt","content":null}`},
		{"write_file", "content", `{"path":"new/empty.txt"}`},
		{"write_file", "content", `{"path":"new/empty.txt","content":null}`},
		{"edit_file", "new_string", `{"path":"existing.txt","old_string":"original"}`},
		{"edit_file", "new_string", `{"path":"existing.txt","old_string":"original","new_string":null}`},
		{"apply_patch", "patch", `{}`},
		{"apply_patch", "patch", `{"patch":null}`},
		{"apply_patch", "patch", `{"patch":""}`},
		{"apply_patch", "patch", `{"patch":" \n\t"}`},
	} {
		t.Run(tt.tool+"/"+tt.input, func(t *testing.T) {
			registry, callCtx := workspaceToolTestRegistry(t)
			path := writeWorkspaceFixture(t, callCtx, "existing.txt", "original\n")
			if _, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"existing.txt"}]}`)); err != nil {
				t.Fatal(err)
			}
			_, err := registry.Execute(context.Background(), callCtx, tt.tool, json.RawMessage(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.field+" is required") {
				t.Fatalf("expected required-field error, got %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != "original\n" {
				t.Fatalf("rejected call changed file: %q, %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "new")); !os.IsNotExist(err) {
				t.Fatalf("rejected call created directories: %v", err)
			}
		})
	}
}

func TestWorkspaceMutationAllowsExplicitEmptyStrings(t *testing.T) {
	for _, tt := range []struct {
		name, tool, input, path, want string
	}{
		{"create empty", "write_file", `{"path":"empty.txt","content":""}`, "empty.txt", ""},
		{"overwrite empty", "write_file", `{"path":"existing.txt","content":""}`, "existing.txt", ""},
		{"delete match", "edit_file", `{"path":"existing.txt","old_string":"original\n","new_string":""}`, "existing.txt", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			registry, callCtx := workspaceToolTestRegistry(t)
			writeWorkspaceFixture(t, callCtx, "existing.txt", "original\n")
			if _, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"existing.txt"}]}`)); err != nil {
				t.Fatal(err)
			}
			if _, err := registry.Execute(context.Background(), callCtx, tt.tool, json.RawMessage(tt.input)); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, tt.path))
			if err != nil || string(got) != tt.want {
				t.Fatalf("content = %q, err = %v", got, err)
			}
		})
	}
}
