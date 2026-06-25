package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestCodexAdapterExecutesConfiguredCommand(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex-adapter")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"assistant_message\":\"from command\",\"output_summary\":{\"runtime\":\"codex\"}}'\n"), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent:   &agentcore.Agent{Name: "Codex"},
		Run: &agentcore.AgentRun{
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "ticket", ID: "T-1"},
			Input:       agentcore.RunInput{Instructions: "summarize"},
			RuntimeKind: agentcore.RuntimeCodex,
		},
	})
	if err != nil {
		t.Fatalf("execute command: %v", err)
	}
	if result.AssistantMessage != "from command" {
		t.Fatalf("unexpected assistant message: %q", result.AssistantMessage)
	}
	if !strings.Contains(string(result.OutputSummary), `"runtime":"codex"`) {
		t.Fatalf("unexpected output summary: %s", string(result.OutputSummary))
	}
}

func TestCodexAdapterUsesWorkspaceLeaseRootAsWorkDir(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	command := filepath.Join(tmp, "codex-adapter")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ncat >/dev/null\nprintf '{\"assistant_message\":\"%s\"}' \"$PWD\"\n"), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent:   &agentcore.Agent{Name: "Codex"},
		Run: &agentcore.AgentRun{
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
			Input:       agentcore.RunInput{Instructions: "change code"},
			RuntimeKind: agentcore.RuntimeCodex,
		},
		WorkspaceLease: &agentcore.WorkspaceLease{ID: "lease-1", RootPath: repo},
	})
	if err != nil {
		t.Fatalf("execute command: %v", err)
	}
	expectedWorkDir, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("resolve expected workdir: %v", err)
	}
	actualWorkDir, err := filepath.EvalSymlinks(result.AssistantMessage)
	if err != nil {
		t.Fatalf("resolve actual workdir: %v", err)
	}
	if actualWorkDir != expectedWorkDir {
		t.Fatalf("expected workspace workdir %q, got %q", expectedWorkDir, actualWorkDir)
	}
}

func TestCodexAdapterCommandSyncsStagedSkillsIntoCodexHome(t *testing.T) {
	tmp := t.TempDir()
	stagedRoot := filepath.Join(tmp, "staged")
	if err := os.MkdirAll(filepath.Join(stagedRoot, "01-demo"), 0o755); err != nil {
		t.Fatalf("mkdir staged skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagedRoot, "01-demo", "SKILL.md"), []byte("demo skill"), 0o644); err != nil {
		t.Fatalf("write staged skill: %v", err)
	}
	command := filepath.Join(tmp, "codex-adapter")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ncat >/dev/null\nif [ -f \"$CODEX_HOME/skills/agent-runtime/01-demo/SKILL.md\" ]; then msg=synced; else msg=missing; fi\nprintf '{\"assistant_message\":\"%s\"}' \"$msg\"\n"), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		RuntimeRoot: filepath.Join(tmp, "runtime"),
		Timeout:     time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent:   &agentcore.Agent{Name: "Codex"},
		Run: &agentcore.AgentRun{
			ID:          "run-command-sync",
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
			Input:       agentcore.RunInput{Instructions: "change code"},
			RuntimeKind: agentcore.RuntimeCodex,
		},
		StagedSkillRoot: stagedRoot,
	})
	if err != nil {
		t.Fatalf("execute command: %v", err)
	}
	if result.AssistantMessage != "synced" {
		t.Fatalf("expected command codex skills to be synced, got %q", result.AssistantMessage)
	}
}

func TestCodexAdapterCommandMasksRepoSkillRootsDuringRun(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agents", "skills", "repo-agent"), 0o755); err != nil {
		t.Fatalf("mkdir .agents skills: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".codex", "skills", "repo-codex"), 0o755); err != nil {
		t.Fatalf("mkdir .codex skills: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agents", "skills", "repo-agent", "SKILL.md"), []byte("repo agent skill"), 0o644); err != nil {
		t.Fatalf("write .agents skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".codex", "skills", "repo-codex", "SKILL.md"), []byte("repo codex skill"), 0o644); err != nil {
		t.Fatalf("write .codex skill: %v", err)
	}
	command := filepath.Join(tmp, "codex-adapter")
	script := `#!/bin/sh
cat >/dev/null
if [ ! -e ".agents/skills" ] && [ -d ".agents/.agent-runtime-hidden-skills-run-mask" ] && [ ! -e ".codex/skills" ] && [ -d ".codex/.agent-runtime-hidden-skills-run-mask" ]; then msg=masked; else msg=visible; fi
printf '{"assistant_message":"%s"}' "$msg"
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     repo,
		Timeout:     time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent:   &agentcore.Agent{Name: "Codex"},
		Run: &agentcore.AgentRun{
			ID:          "run-mask",
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
			Input:       agentcore.RunInput{Instructions: "change code"},
			RuntimeKind: agentcore.RuntimeCodex,
		},
	})
	if err != nil {
		t.Fatalf("execute command: %v", err)
	}
	if result.AssistantMessage != "masked" {
		t.Fatalf("expected repo skill roots to be masked during command run, got %q", result.AssistantMessage)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agents", "skills", "repo-agent", "SKILL.md")); err != nil {
		t.Fatalf("expected .agents skills to be restored, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".codex", "skills", "repo-codex", "SKILL.md")); err != nil {
		t.Fatalf("expected .codex skills to be restored, stat err=%v", err)
	}
}

func TestCodexAdapterExecutesAppServerTurn(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"hello"}}'
printf '%s\n' '{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"totalTokens":3,"inputTokens":1,"cachedInputTokens":0,"outputTokens":2},"last":{}}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     time.Second,
		AppServer:   true,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent:   &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run: &agentcore.AgentRun{
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
			Input:       agentcore.RunInput{Instructions: "change code"},
			RuntimeKind: agentcore.RuntimeCodex,
		},
	})
	if err != nil {
		t.Fatalf("execute app-server: %v", err)
	}
	if result.AssistantMessage != "hello" {
		t.Fatalf("unexpected assistant message: %q", result.AssistantMessage)
	}
	if !strings.Contains(string(result.OutputSummary), `"output_tokens":2`) {
		t.Fatalf("expected usage in output summary, got %s", string(result.OutputSummary))
	}
}

func TestCodexAdapterSyncsStagedSkillsIntoCodexHome(t *testing.T) {
	tmp := t.TempDir()
	stagedRoot := filepath.Join(tmp, "staged")
	if err := os.MkdirAll(filepath.Join(stagedRoot, "01-demo"), 0o755); err != nil {
		t.Fatalf("mkdir staged skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagedRoot, "01-demo", "SKILL.md"), []byte("demo skill"), 0o644); err != nil {
		t.Fatalf("write staged skill: %v", err)
	}
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
if [ -f "$CODEX_HOME/skills/agent-runtime/01-demo/SKILL.md" ]; then status="synced"; else status="missing"; fi
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' "{\"method\":\"item/agentMessage/delta\",\"params\":{\"threadId\":\"thread-1\",\"turnId\":\"turn-1\",\"itemId\":\"item-1\",\"delta\":\"$status\"}}"
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		RuntimeRoot: filepath.Join(tmp, "runtime"),
		Timeout:     time.Second,
		AppServer:   true,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent:   &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run: &agentcore.AgentRun{
			ID:          "run-sync",
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
			Input:       agentcore.RunInput{Instructions: "change code"},
			RuntimeKind: agentcore.RuntimeCodex,
		},
		Store:           store.NewMemory(),
		StagedSkillRoot: stagedRoot,
	})
	if err != nil {
		t.Fatalf("execute app-server: %v", err)
	}
	if result.AssistantMessage != "synced" {
		t.Fatalf("expected staged skills to be synced into codex home, got %q", result.AssistantMessage)
	}
}

func TestCodexAdapterAppServerMasksRepoSkillRootsDuringRun(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agents", "skills", "repo-agent"), 0o755); err != nil {
		t.Fatalf("mkdir .agents skills: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".codex", "skills", "repo-codex"), 0o755); err != nil {
		t.Fatalf("mkdir .codex skills: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agents", "skills", "repo-agent", "SKILL.md"), []byte("repo agent skill"), 0o644); err != nil {
		t.Fatalf("write .agents skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".codex", "skills", "repo-codex", "SKILL.md"), []byte("repo codex skill"), 0o644); err != nil {
		t.Fatalf("write .codex skill: %v", err)
	}
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
if [ ! -e ".agents/skills" ] && [ -d ".agents/.agent-runtime-hidden-skills-run-mask-appserver" ] && [ ! -e ".codex/skills" ] && [ -d ".codex/.agent-runtime-hidden-skills-run-mask-appserver" ]; then status="masked"; else status="visible"; fi
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' "{\"method\":\"item/agentMessage/delta\",\"params\":{\"threadId\":\"thread-1\",\"turnId\":\"turn-1\",\"itemId\":\"msg-1\",\"delta\":\"$status\"}}"
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     repo,
		Timeout:     time.Second,
		AppServer:   true,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   store.NewMemory(),
		Agent:   &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run: &agentcore.AgentRun{
			ID:          "run-mask-appserver",
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
			Input:       agentcore.RunInput{Instructions: "change code"},
			RuntimeKind: agentcore.RuntimeCodex,
		},
	})
	if err != nil {
		t.Fatalf("execute app-server: %v", err)
	}
	if result.AssistantMessage != "masked" {
		t.Fatalf("expected repo skill roots to be masked during app-server run, got %q", result.AssistantMessage)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agents", "skills", "repo-agent", "SKILL.md")); err != nil {
		t.Fatalf("expected .agents skills to be restored, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".codex", "skills", "repo-codex", "SKILL.md")); err != nil {
		t.Fatalf("expected .codex skills to be restored, stat err=%v", err)
	}
}

func TestPrepareCodexHomeRefreshesSyncedSkillRoot(t *testing.T) {
	tmp := t.TempDir()
	stagedRoot := filepath.Join(tmp, "staged")
	if err := os.MkdirAll(filepath.Join(stagedRoot, "01-new"), 0o755); err != nil {
		t.Fatalf("mkdir staged skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagedRoot, "01-new", "SKILL.md"), []byte("new skill"), 0o644); err != nil {
		t.Fatalf("write staged skill: %v", err)
	}
	codexHome := filepath.Join(tmp, "home", ".codex")
	if err := os.MkdirAll(filepath.Join(codexHome, "skills", "agent-runtime", "01-old"), 0o755); err != nil {
		t.Fatalf("mkdir old skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "skills", "agent-runtime", "01-old", "SKILL.md"), []byte("old skill"), 0o644); err != nil {
		t.Fatalf("write old skill: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{RuntimeRoot: filepath.Join(tmp, "runtime")})
	state := &codexSessionState{HomeRoot: filepath.Join(tmp, "home"), CodexHome: codexHome}

	if err := adapter.prepareCodexHome(context.Background(), &ExecutionContext{
		Agent:           &agentcore.Agent{Name: "Codex"},
		Run:             &agentcore.AgentRun{ID: "run-refresh", AppID: "app-a"},
		StagedSkillRoot: stagedRoot,
	}, state); err != nil {
		t.Fatalf("prepare codex home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "skills", "agent-runtime", "01-new", "SKILL.md")); err != nil {
		t.Fatalf("expected new synced skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "skills", "agent-runtime", "01-old", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("expected old synced skill to be removed, stat err=%v", err)
	}
}

func TestCodexAdapterMapsAppServerEventsToArtifactsAndEvents(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' '{"method":"thread/started","params":{"thread":{"id":"thread-1","cwd":"/tmp"}}}'
printf '%s\n' '{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' '{"method":"turn/plan/updated","params":{"threadId":"thread-1","turnId":"turn-1","explanation":"next steps","plan":[{"step":"inspect","status":"completed"},{"step":"patch","status":"inProgress"}]}}'
printf '%s\n' '{"method":"item/started","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"cmd-1","command":"go test ./...","cwd":"/tmp","status":"running"}}}'
printf '%s\n' '{"method":"item/commandExecution/outputDelta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"cmd-1","delta":"ok\n"}}'
printf '%s\n' '{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"commandExecution","id":"cmd-1","command":"go test ./...","cwd":"/tmp","status":"completed","aggregatedOutput":"ok\n","exitCode":0,"durationMs":12}}}'
printf '%s\n' '{"method":"turn/diff/updated","params":{"threadId":"thread-1","turnId":"turn-1","diff":"diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new"}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"msg-1","delta":"done"}}'
printf '%s\n' '{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"totalTokens":5,"inputTokens":2,"cachedInputTokens":1,"outputTokens":3,"reasoningOutputTokens":1},"last":{}}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-events",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Instructions: "change code"},
		RuntimeKind: agentcore.RuntimeCodex,
	}
	eventSink := &testEventSink{}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     2 * time.Second,
		AppServer:   true,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run:     run,
		ArtifactWriter: testArtifactWriter{
			store: mem,
			run:   run,
		},
		EventSink: eventSink,
	})
	if err != nil {
		t.Fatalf("execute app-server: %v", err)
	}
	if result.AssistantMessage != "done" {
		t.Fatalf("unexpected assistant message: %q", result.AssistantMessage)
	}
	for _, expected := range []string{
		`"output_tokens":3`,
		`"reasoning_output_tokens":1`,
		`"tool_summaries":[{"id":"cmd-1","name":"run_command"`,
		`"run_plan":{"note":"next steps"`,
	} {
		if !strings.Contains(string(result.OutputSummary), expected) {
			t.Fatalf("expected %s in output summary, got %s", expected, string(result.OutputSummary))
		}
	}

	artifacts, err := mem.ListArtifacts(context.Background(), "app-a", "run-events")
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	wantArtifacts := map[string]bool{
		"codex_config":       false,
		"run_plan":           false,
		"codex_stdout_chunk": false,
		"codex_stdout":       false,
		"codex_diff":         false,
	}
	for _, artifact := range artifacts {
		if _, ok := wantArtifacts[artifact.ArtifactType]; ok {
			wantArtifacts[artifact.ArtifactType] = true
		}
	}
	for artifactType, found := range wantArtifacts {
		if !found {
			t.Fatalf("expected artifact %s, got %#v", artifactType, artifacts)
		}
	}
	if !eventSink.hasType("plan_updated") || !eventSink.hasType("tool_call_started") || !eventSink.hasType("tool_call_finished") {
		t.Fatalf("expected plan/tool live events, got %#v", eventSink.events)
	}
	toolCalls, err := mem.ListToolCalls(context.Background(), "app-a", "run-events")
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected one durable tool call, got %#v", toolCalls)
	}
	if toolCalls[0].ToolName != "run_command" || !toolCalls[0].Mutating || toolCalls[0].Error != "" {
		t.Fatalf("unexpected durable tool call: %#v", toolCalls[0])
	}
	if !strings.Contains(string(toolCalls[0].Input), `"codex_item_id":"cmd-1"`) || !strings.Contains(string(toolCalls[0].Output), `"summary":"ok"`) {
		t.Fatalf("expected codex input/output details, got input=%s output=%s", string(toolCalls[0].Input), string(toolCalls[0].Output))
	}
}

func TestCodexAdapterMapsFailedTurnToErrorAndStderr(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
IFS= read -r line
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
printf '%s\n' '{"method":"error","params":{"threadId":"thread-1","turnId":"turn-1","error":{"message":"tool crashed"},"willRetry":false}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"failed","error":{"message":"model failed"}}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-failed",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Instructions: "change code"},
		RuntimeKind: agentcore.RuntimeCodex,
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     2 * time.Second,
		AppServer:   true,
	})

	_, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run:     run,
		ArtifactWriter: testArtifactWriter{
			store: mem,
			run:   run,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "codex turn failed: model failed") {
		t.Fatalf("expected failed turn error, got %v", err)
	}
	artifacts, err := mem.ListArtifacts(context.Background(), "app-a", "run-failed")
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	foundStderr := false
	for _, artifact := range artifacts {
		if artifact.ArtifactType == "codex_stderr" && strings.Contains(artifact.InlineContent, "model failed") {
			foundStderr = true
		}
	}
	if !foundStderr {
		t.Fatalf("expected stderr artifact, got %#v", artifacts)
	}
}

func TestCodexAdapterPersistsAndResumesPendingApproval(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex")
	script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
if [ "$RESUME" != "1" ]; then
  IFS= read -r line
  printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
  IFS= read -r line
  printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1","status":"running"}}}'
  printf '%s\n' '{"id":4,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","command":"git status","cwd":"/tmp"}}'
  sleep 1
else
  IFS= read -r line
  printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-1","cwd":"/tmp"},"model":"gpt","modelProvider":"openai"}}'
  printf '%s\n' '{"id":4,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","command":"git status","cwd":"/tmp"}}'
  IFS= read -r line
  printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"msg-1","delta":"approved"}}'
  printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}'
  sleep 1
fi
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Env:         []string{"RESUME=0"},
		Timeout:     2 * time.Second,
		AppServer:   true,
	})
	run := &agentcore.AgentRun{
		ID:          "run-1",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Instructions: "change code"},
		RuntimeKind: agentcore.RuntimeCodex,
	}
	execCtx := &ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run:     run,
		InteractionBroker: testInteractionBroker{
			store: mem,
			run:   run,
		},
	}

	first, err := adapter.Execute(execCtx)
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	if !first.WaitForApproval {
		t.Fatalf("expected approval pause, got %#v", first)
	}
	artifacts, err := mem.ListArtifacts(context.Background(), "app-a", "run-1")
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts) == 0 || !strings.Contains(artifacts[len(artifacts)-1].InlineContent, `"pending_request"`) {
		t.Fatalf("expected pending session artifact, got %#v", artifacts)
	}
	interactions, err := mem.ListInteractions(context.Background(), "app-a", "run-1")
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != "human_approval" {
		t.Fatalf("expected approval interaction, got %#v", interactions)
	}

	run.Input.Metadata = map[string]interface{}{
		"last_resume": map[string]interface{}{
			"intent": "approve",
		},
	}
	resumeAdapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Env:         []string{"RESUME=1"},
		Timeout:     2 * time.Second,
		AppServer:   true,
	})
	second, err := resumeAdapter.Execute(execCtx)
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if second.AssistantMessage != "approved" {
		t.Fatalf("expected resumed assistant message, got %q", second.AssistantMessage)
	}
	artifacts, err = mem.ListArtifacts(context.Background(), "app-a", "run-1")
	if err != nil {
		t.Fatalf("list artifacts after resume: %v", err)
	}
	foundClear := false
	for _, artifact := range artifacts {
		var state codexSessionState
		if artifact.ArtifactType == codexSessionStateArtifactType && json.Unmarshal([]byte(artifact.InlineContent), &state) == nil && state.ClearedAt != nil {
			foundClear = true
		}
	}
	if !foundClear {
		t.Fatalf("expected cleared session artifact, got %#v", artifacts)
	}
}

type testInteractionBroker struct {
	store agentcore.Store
	run   *agentcore.AgentRun
}

func (b testInteractionBroker) RequestInteraction(ctx context.Context, interaction agentcore.AgentRunInteraction) error {
	interaction.AppID = b.run.AppID
	interaction.RunID = b.run.ID
	interaction.RuntimeKind = b.run.RuntimeKind
	return b.store.AppendInteraction(ctx, &interaction)
}

type testEventSink struct {
	events []Event
}

func (s *testEventSink) Emit(ctx context.Context, event Event) {
	s.events = append(s.events, event)
}

func (s *testEventSink) hasType(eventType string) bool {
	for _, event := range s.events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func TestResolveCodexLaunchUsesNodeForJSPath(t *testing.T) {
	bin, args, workDir, err := ResolveCodexLaunch("/tmp/codex.js", "/workspace")
	if err != nil {
		t.Fatalf("resolve codex launch: %v", err)
	}
	if bin != "node" || len(args) != 1 || args[0] != "/tmp/codex.js" || workDir != "/workspace" {
		t.Fatalf("unexpected launch: bin=%q args=%#v workDir=%q", bin, args, workDir)
	}
}

func TestCodexDiffFromFileChangeNormalizesAbsoluteWorkdirPath(t *testing.T) {
	workDir := "/tmp/agent-runtime-workspaces/run-123/repo"
	item := CodexThreadItem{
		Type: "fileChange",
		Changes: []CodexFileChange{{
			Path: "/tmp/agent-runtime-workspaces/run-123/repo/frontend/src/lib/utils/imageGeneration.ts",
			Diff: "@@ -1 +1 @@\n-old\n+new",
		}},
	}

	diff := CodexDiffFromFileChange(workDir, item)

	if !strings.Contains(diff, "--- a/frontend/src/lib/utils/imageGeneration.ts") {
		t.Fatalf("expected repo-relative diff header, got %q", diff)
	}
	if strings.Contains(diff, "/tmp/agent-runtime-workspaces/") {
		t.Fatalf("expected absolute temp path to be stripped, got %q", diff)
	}
}

func TestNormalizeCodexUnifiedDiffStripsAbsoluteHeaders(t *testing.T) {
	workDir := "/tmp/agent-runtime-workspaces/run-123/repo"
	raw := strings.Join([]string{
		"diff --git a//tmp/agent-runtime-workspaces/run-123/repo/frontend/src/lib/utils/imageGeneration.ts b//tmp/agent-runtime-workspaces/run-123/repo/frontend/src/lib/utils/imageGeneration.ts",
		"--- a//tmp/agent-runtime-workspaces/run-123/repo/frontend/src/lib/utils/imageGeneration.ts",
		"+++ b//tmp/agent-runtime-workspaces/run-123/repo/frontend/src/lib/utils/imageGeneration.ts",
		"@@ -1 +1 @@",
		"-old",
		"+new",
	}, "\n")

	got := NormalizeCodexUnifiedDiff(workDir, raw)

	if strings.Contains(got, "/tmp/agent-runtime-workspaces/") {
		t.Fatalf("expected absolute temp paths to be removed from unified diff, got %q", got)
	}
	if !strings.Contains(got, "diff --git a/frontend/src/lib/utils/imageGeneration.ts b/frontend/src/lib/utils/imageGeneration.ts") {
		t.Fatalf("expected normalized git header, got %q", got)
	}
}
