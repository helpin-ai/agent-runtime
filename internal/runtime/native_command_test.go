package runtime

import (
	"context"
	"encoding/json"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"os/exec"
	"strings"
	"testing"
)

func TestNativeFailedCommandRetainsErrorAndCompactedTail(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	x := contextTestExec(t)
	x.Agent.ApprovalMode = agentcore.ApprovalModeNever
	x.AllowedTools = map[string]bool{"run_command": true}
	x.WorkspaceLease = &agentcore.WorkspaceLease{RootPath: t.TempDir()}
	x.Run.WorkspaceLease = x.WorkspaceLease
	input, _ := json.Marshal(map[string]any{"program": "python3", "args": []string{"-c", "print('x'*60000); print('FINAL FAILURE'); raise SystemExit(3)"}})
	got := executeSingleNativeToolCall(context.Background(), x, NativeBlock{ToolCallID: "cmd", ToolName: "run_command", Input: input})
	if !got.IsError || !strings.Contains(got.Output, "FINAL FAILURE") || !strings.Contains(got.Output, "exit status 3") {
		t.Fatalf("invalid command outcome: error=%v output=%s", got.IsError, got.Output)
	}
	visible := analyzeNativeToolOutputForModel("run_command", got.Output)
	if !strings.Contains(visible.Content, "FINAL FAILURE") || !strings.Contains(visible.Content, "exit status 3") {
		t.Fatal("model compaction lost final command error")
	}
}
