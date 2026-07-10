package workspace

import (
	"encoding/json"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestWorkspaceExecutionConfigSelectors(t *testing.T) {
	agent := &agentcore.Agent{ExecutionConfig: json.RawMessage(`{
		"workspace":{"mode":"repository","access":"read_only"}
	}`)}
	if got := WorkspaceMode(agent); got != ModeRepository {
		t.Fatalf("WorkspaceMode() = %q, want %q", got, ModeRepository)
	}
	if got := AccessMode(agent); got != AccessReadOnly {
		t.Fatalf("AccessMode() = %q, want %q", got, AccessReadOnly)
	}
}

func TestWorkspaceExecutionConfigSelectorsRejectInvalidJSON(t *testing.T) {
	agent := &agentcore.Agent{ExecutionConfig: json.RawMessage(`{`)}
	if got := WorkspaceMode(agent); got != "" {
		t.Fatalf("WorkspaceMode() = %q, want empty", got)
	}
	if got := AccessMode(agent); got != "" {
		t.Fatalf("AccessMode() = %q, want empty", got)
	}
}
