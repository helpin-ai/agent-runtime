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

func TestNativeManagedReadBudgetAndCursor(t *testing.T) {
	registry, callCtx := workspaceToolTestRegistry(t)
	if err := os.WriteFile(filepath.Join(callCtx.Run.WorkspaceLease.RootPath, "source.txt"), []byte(strings.Repeat(strings.Repeat("x", 199)+"\n", 100)), 0600); err != nil {
		t.Fatal(err)
	}
	ends := []int{}
	for _, enabled := range []bool{false, true} {
		callCtx.Agent = &agentcore.Agent{RuntimeKind: agentcore.RuntimeNativeSDK}
		if enabled {
			callCtx.Agent.ExecutionConfig = json.RawMessage(`{"native_context":{"enabled":true,"context_window":128000}}`)
		}
		output, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"source.txt","limit_lines":100}]}`))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Files []struct {
				EndLine       int  `json:"end_line"`
				NextStartLine int  `json:"next_start_line"`
				HasMore       bool `json:"has_more"`
			} `json:"files"`
		}
		if err := json.Unmarshal(output, &result); err != nil || len(result.Files) != 1 {
			t.Fatalf("result: %s %v", output, err)
		}
		file := result.Files[0]
		if !file.HasMore || file.NextStartLine != file.EndLine+1 {
			t.Fatalf("broken cursor: %+v", file)
		}
		ends = append(ends, file.EndLine)
	}
	if ends[1] < ends[0]*3 {
		t.Fatalf("larger budget not effective: %v", ends)
	}
	callCtx.Agent.RuntimeKind = agentcore.RuntimeCodex
	if nativeManagedReadBudget(callCtx) {
		t.Fatal("changed non-native read behavior")
	}
}
