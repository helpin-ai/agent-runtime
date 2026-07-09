package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestSQLStoreAgentAndRunAppIsolation(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLStore(t)

	agent := &agentcore.Agent{
		AppID:                 "app-a",
		Name:                  "Planner",
		RuntimeKind:           agentcore.RuntimeNativeSDK,
		AllowedTools:          []string{"get_context"},
		AllowedTargets:        []string{"ticket"},
		ApprovalMode:          agentcore.ApprovalModeNever,
		DefaultInvocationMode: agentcore.InvocationAutonomous,
	}
	if err := store.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	got, err := store.GetAgent(ctx, "app-a", agent.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got == nil || got.ID != agent.ID {
		t.Fatalf("expected app-a agent, got %#v", got)
	}
	got, err = store.GetAgent(ctx, "app-b", agent.ID)
	if err != nil {
		t.Fatalf("get other app agent: %v", err)
	}
	if got != nil {
		t.Fatalf("expected app isolation, got %#v", got)
	}

	run := &agentcore.AgentRun{
		AppID:     "app-a",
		HostRunID: "host-run-1",
		AgentID:   agent.ID,
		Target:    agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Input:     agentcore.RunInput{Instructions: "triage"},
		WorkspaceLease: &agentcore.WorkspaceLease{
			ID:            "lease-1",
			Provider:      "test",
			RootPath:      "/tmp/repo",
			CleanupPolicy: "on_terminal",
			Metadata: map[string]interface{}{
				"branch": "task/test",
			},
		},
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.Status != agentcore.RunStatusQueued {
		t.Fatalf("run status = %q", run.Status)
	}

	runs, err := store.ListRuns(ctx, "app-a")
	if err != nil {
		t.Fatalf("list app-a runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("expected app-a run, got %#v", runs)
	}
	if runs[0].WorkspaceLease == nil || runs[0].WorkspaceLease.ID != "lease-1" || runs[0].WorkspaceLease.RootPath != "/tmp/repo" {
		t.Fatalf("workspace lease was not persisted: %#v", runs[0].WorkspaceLease)
	}
	byHost, err := store.GetRunByHostRunID(ctx, "app-a", "host-run-1")
	if err != nil {
		t.Fatalf("get run by host id: %v", err)
	}
	if byHost == nil || byHost.ID != run.ID {
		t.Fatalf("expected host run lookup to return %q, got %#v", run.ID, byHost)
	}
	if err := store.CreateRun(ctx, &agentcore.AgentRun{
		AppID:     "app-a",
		HostRunID: "host-run-1",
		AgentID:   agent.ID,
		Target:    agentcore.TargetRef{Type: "ticket", ID: "T-2"},
	}); err == nil {
		t.Fatal("expected duplicate host_run_id error")
	}
	runs, err = store.ListRuns(ctx, "app-b")
	if err != nil {
		t.Fatalf("list app-b runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected no app-b runs, got %#v", runs)
	}
}

func TestSQLStoreAppendsMessagesAndArtifactsInSequence(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLStore(t)

	for _, content := range []string{"first", "second"} {
		message := &agentcore.AgentRunMessage{
			AppID:       "app-a",
			RunID:       "run-1",
			Role:        "assistant",
			Content:     content,
			MessageType: "assistant_message",
		}
		if err := store.AppendMessage(ctx, message); err != nil {
			t.Fatalf("append message %q: %v", content, err)
		}
	}
	messages, err := store.ListMessages(ctx, "app-a", "run-1")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 2 || messages[0].SequenceNo != 1 || messages[1].SequenceNo != 2 {
		t.Fatalf("messages not sequenced: %#v", messages)
	}
	correlated := &agentcore.AgentRunMessage{
		AppID: "app-a", RunID: "run-1", RuntimeMessageID: "resume-1",
		Role: "user", Content: "continue", MessageType: "message",
	}
	if err := store.AppendMessage(ctx, correlated); err != nil {
		t.Fatalf("append correlated message: %v", err)
	}
	duplicate := &agentcore.AgentRunMessage{
		AppID: "app-a", RunID: "run-1", RuntimeMessageID: "resume-1",
		Role: "user", Content: "continue", MessageType: "message",
	}
	if err := store.AppendMessage(ctx, duplicate); err != nil {
		t.Fatalf("append duplicate correlated message: %v", err)
	}
	messages, err = store.ListMessages(ctx, "app-a", "run-1")
	if err != nil {
		t.Fatalf("list messages after correlated duplicate: %v", err)
	}
	if len(messages) != 3 || duplicate.ID != correlated.ID || duplicate.SequenceNo != correlated.SequenceNo {
		t.Fatalf("expected correlated message append to be idempotent: first=%#v duplicate=%#v all=%#v", correlated, duplicate, messages)
	}

	for _, artifactType := range []string{"plan", "tool_log"} {
		artifact := &agentcore.AgentRunArtifact{
			AppID:        "app-a",
			RunID:        "run-1",
			ArtifactType: artifactType,
			Format:       "text",
		}
		if err := store.AppendArtifact(ctx, artifact); err != nil {
			t.Fatalf("append artifact %q: %v", artifactType, err)
		}
	}
	artifacts, err := store.ListArtifacts(ctx, "app-a", "run-1")
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts) != 2 || artifacts[0].SequenceNo != 1 || artifacts[1].SequenceNo != 2 {
		t.Fatalf("artifacts not sequenced: %#v", artifacts)
	}
	if artifacts[0].StorageMode != "inline" {
		t.Fatalf("expected default inline storage mode, got %q", artifacts[0].StorageMode)
	}
}

func TestSQLStoreSanitizesMessageAndJSONFields(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLStore(t)

	message := &agentcore.AgentRunMessage{
		AppID:           "app-a",
		RunID:           "run-1",
		Role:            "assistant",
		Content:         "created task\x00with scanner output",
		MessageType:     "assistant_message",
		ContentBlocks:   json.RawMessage(`[{"type":"tool_result","text":"secret\u0000match"}]`),
		ToolInvocations: json.RawMessage(`[{"tool_name":"scan","result":{"bad\u0000key":"finding\u0000value"}}]`),
	}
	if err := store.AppendMessage(ctx, message); err != nil {
		t.Fatalf("append message: %v", err)
	}
	if strings.ContainsRune(message.Content, '\x00') {
		t.Fatalf("content still contains null character: %q", message.Content)
	}
	for name, raw := range map[string]json.RawMessage{
		"content_blocks":   message.ContentBlocks,
		"tool_invocations": message.ToolInvocations,
	} {
		assertSanitizedJSON(t, name, raw)
	}

	artifact := &agentcore.AgentRunArtifact{
		AppID:         "app-a",
		RunID:         "run-1",
		ArtifactType:  "tool_log",
		Format:        "text",
		InlineContent: "Tool output with binary\x00data",
		Metadata:      json.RawMessage(`{"bad\u0000key":"bad\u0000value"}`),
	}
	if err := store.AppendArtifact(ctx, artifact); err != nil {
		t.Fatalf("append artifact: %v", err)
	}
	if strings.ContainsRune(artifact.InlineContent, '\x00') {
		t.Fatalf("inline content was not sanitized: %q", artifact.InlineContent)
	}
	assertSanitizedJSON(t, "artifact metadata", artifact.Metadata)

	call := &agentcore.ToolCall{
		AppID:    "app-a",
		RunID:    "run-1",
		ToolName: "scan",
		Input:    json.RawMessage(`{"query":"bad\u0000input"}`),
		Output:   json.RawMessage(`{"result":"bad\u0000output"}`),
		Error:    "failed on byte\x00",
	}
	if err := store.AppendToolCall(ctx, call); err != nil {
		t.Fatalf("append tool call: %v", err)
	}
	assertSanitizedJSON(t, "tool input", call.Input)
	assertSanitizedJSON(t, "tool output", call.Output)
	if strings.ContainsRune(call.Error, '\x00') {
		t.Fatalf("tool error was not sanitized: %q", call.Error)
	}
	calls, err := store.ListToolCalls(ctx, "app-a", "run-1")
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(calls) != 1 || calls[0].ToolName != "scan" || calls[0].Error != call.Error {
		t.Fatalf("unexpected listed tool calls: %#v", calls)
	}
	assertSanitizedJSON(t, "listed tool input", calls[0].Input)
	assertSanitizedJSON(t, "listed tool output", calls[0].Output)
}

func TestSanitizePostgresJSONRawMessageDropsInvalidJSON(t *testing.T) {
	raw := sanitizePostgresJSONRawMessage(json.RawMessage(`{"unterminated"`), nil)
	if raw != nil {
		t.Fatalf("expected invalid json to be dropped, got %s", string(raw))
	}
	raw = sanitizePostgresJSONRawMessage(json.RawMessage(`{"unterminated"`), json.RawMessage(`{}`))
	if string(raw) != "{}" {
		t.Fatalf("expected invalid json to use fallback, got %s", string(raw))
	}
}

func TestAutoMigrateCreatesCodexAuthTokensTable(t *testing.T) {
	sqlStore := newTestSQLStore(t)
	if !sqlStore.DB().Migrator().HasTable("codex_auth_tokens") {
		t.Fatalf("expected codex_auth_tokens table")
	}
}

func newTestSQLStore(t *testing.T) *SQL {
	t.Helper()
	sqlStore, err := OpenSQL(SQLConfig{
		Driver: "sqlite",
		DSN:    fmt.Sprintf("file:agent_runtime_store_%d?mode=memory&cache=shared", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("open sql store: %v", err)
	}
	if err := sqlStore.AutoMigrate(); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return sqlStore
}

func assertSanitizedJSON(t *testing.T, name string, raw json.RawMessage) {
	t.Helper()
	if strings.Contains(string(raw), `\u0000`) {
		t.Fatalf("%s still contains postgres-rejected null escape: %s", name, string(raw))
	}
	if strings.ContainsRune(string(raw), '\x00') {
		t.Fatalf("%s still contains raw null character: %s", name, string(raw))
	}
	if !json.Valid(raw) {
		t.Fatalf("%s is not valid json after sanitization: %s", name, string(raw))
	}
}
