package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestReadSymbolManagedBudgetMatchesRangeRead(t *testing.T) {
	source := "package p\n\nfunc Large() {\n" + strings.Repeat("\tprintln(\"a reasonably long line inside this declaration\")\n", 80) + "}\n"
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint("managed=", enabled), func(t *testing.T) {
			registry, callCtx := workspaceToolTestRegistry(t)
			callCtx.Agent = &agentcore.Agent{RuntimeKind: agentcore.RuntimeNativeSDK}
			if enabled {
				callCtx.Agent.ExecutionConfig = json.RawMessage(`{"native_context":{"enabled":true,"context_window":128000}}`)
			}
			writeWorkspaceFixture(t, callCtx, "large.go", source)
			symbol, err := registry.Execute(context.Background(), callCtx, "read_symbol", json.RawMessage(`{"path":"large.go","symbol":"Large"}`))
			if err != nil {
				t.Fatal(err)
			}
			rangeRead, err := registry.Execute(context.Background(), callCtx, "read_files", json.RawMessage(`{"files":[{"path":"large.go","start_line":3,"limit_lines":82}]}`))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Files []struct {
					Content       string `json:"content"`
					HasMore       bool   `json:"has_more"`
					EndLine       int    `json:"end_line"`
					NextStartLine int    `json:"next_start_line"`
				} `json:"files"`
			}
			if err := json.Unmarshal(rangeRead, &result); err != nil || len(result.Files) != 1 {
				t.Fatalf("invalid range read: %s, %v", rangeRead, err)
			}
			file := result.Files[0]
			_, body, ok := strings.Cut(ToolResultText(symbol), "\n")
			if !ok || strings.TrimSpace(body) != strings.TrimSpace(file.Content) {
				t.Fatalf("symbol and range read differ:\n%s\n%s", symbol, rangeRead)
			}
			if file.HasMore == enabled {
				t.Fatalf("expected truncation only without managed context: %s", rangeRead)
			}
			if enabled && file.EndLine != 84 {
				t.Fatalf("managed read missed declaration end: %s", rangeRead)
			}
			if !enabled && (file.NextStartLine != file.EndLine+1 || !strings.Contains(body, fmt.Sprintf("start_line=%d", file.NextStartLine))) {
				t.Fatalf("invalid symbol continuation: %s", symbol)
			}
		})
	}
}
