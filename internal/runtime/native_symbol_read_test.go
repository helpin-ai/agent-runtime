package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestNativeSymbolReadReachesModelIntact(t *testing.T) {
	for _, lines := range []int{80, 180} {
		t.Run(fmt.Sprint(lines), func(t *testing.T) {
			x := contextTestExec(t)
			x.Agent.AllowedTools = []string{"read_symbol"}
			x.AllowedTools = map[string]bool{"read_symbol": true}
			x.Run.WorkspaceLease = &agentcore.WorkspaceLease{RootPath: t.TempDir()}
			source := "package p\n\nfunc Large() {\n" + strings.Repeat("\tprintln(\"a reasonably long line inside this declaration\")\n", lines) + "}\n"
			if err := os.WriteFile(filepath.Join(x.Run.WorkspaceLease.RootPath, "large.go"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			input := json.RawMessage(`{"path":"large.go","symbol":"Large"}`)
			raw, err := x.Tools.Execute(x.Context, toolCallContext(x), "read_symbol", input)
			if err != nil {
				t.Fatal(err)
			}
			output := tools.ToolResultText(raw)
			if utf8.RuneCountInString(output) <= 4000 {
				t.Fatal("fixture must exceed the previous model visibility cap")
			}
			if strings.Contains(output, "start_line=") != (lines == 180) {
				t.Fatalf("unexpected continuation: %s", output)
			}
			history := []NativeMessage{
				{Role: "assistant", Blocks: []NativeBlock{{Type: nativeBlockTypeToolCall, ToolCallID: "read-1", ToolName: "read_symbol", Input: input}}},
				{Role: "tool", Blocks: []NativeBlock{{Type: nativeBlockTypeToolResult, ToolCallID: "read-1", ToolName: "read_symbol", Output: output}}},
			}
			messages, err := nativeMessagesToEino("system", history, nativeToolNameMapper{})
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 3 || messages[2].Content != output {
				t.Fatal("chat model lost symbol lines or continuation")
			}
			agentic, err := nativeMessagesToAgentic("system", history, nativeToolNameMapper{})
			if err != nil {
				t.Fatal(err)
			}
			if len(agentic) != 3 || len(agentic[2].ContentBlocks) != 1 || agentic[2].ContentBlocks[0].FunctionToolResult == nil {
				t.Fatal("missing agentic tool result")
			}
			blocks := agentic[2].ContentBlocks[0].FunctionToolResult.Content
			if len(blocks) != 1 || blocks[0].Text == nil || blocks[0].Text.Text != output {
				t.Fatal("agentic model lost symbol lines or continuation")
			}
		})
	}
}

func TestNativeOversizedSymbolOutputRemainsBounded(t *testing.T) {
	output := strings.Repeat("unexpectedly large output\n", 2000)
	visible := prepareNativeToolResultForModel("read_symbol", output, false)
	if !visible.Compacted || visible.VisibleRunes >= 16*1024 {
		t.Fatal("oversized symbol output bypassed compaction")
	}
}
