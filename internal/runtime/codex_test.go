package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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

func TestCodexAdapterWithoutWorkspaceLeaseUsesIsolatedWorkDir(t *testing.T) {
	current, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	tmp := t.TempDir()
	command := filepath.Join(tmp, "codex-adapter")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ncat >/dev/null\nprintf '{\"assistant_message\":\"%s\"}' \"$PWD\"\n"), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		Timeout:     time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent:   &agentcore.Agent{Name: "Codex"},
		Run: &agentcore.AgentRun{
			ID:          "run-no-lease",
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "workspace", ID: "ws-1"},
			Input:       agentcore.RunInput{Instructions: "inspect context"},
			RuntimeKind: agentcore.RuntimeCodex,
		},
	})
	if err != nil {
		t.Fatalf("execute command: %v", err)
	}
	if result.AssistantMessage == current {
		t.Fatalf("codex used runtime process cwd as workdir: %q", result.AssistantMessage)
	}
	if !strings.Contains(result.AssistantMessage, "agent-runtime-codex-run-no-lease") {
		t.Fatalf("expected isolated codex workdir, got %q", result.AssistantMessage)
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
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"inspect"}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"the"}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"repository"}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"first"}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"."}}'
printf '%s\n' '{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"item-1","text":"inspect the repository first."}}}'
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
	if result.AssistantMessage != "inspect the repository first." {
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

func TestCodexEventMapperBuffersAssistantDeltasUntilCompletedItem(t *testing.T) {
	eventSink := &testEventSink{}
	mapper := newCodexEventMapper(&ExecutionContext{
		AppID:     "app-a",
		Agent:     &agentcore.Agent{Name: "Codex", RuntimeKind: agentcore.RuntimeCodex},
		Run:       &agentcore.AgentRun{ID: "run-codex-stream-words", AppID: "app-a", RuntimeKind: agentcore.RuntimeCodex},
		EventSink: eventSink,
	}, "/tmp")
	tokens := []string{"inspect", "the", "Rust", "crate", "first,", "then", "update", "the", "dependency", "and", "build", "against", "the", "new", "API.", "If", "it", "breaks", "I", "'ll", "patch", "."}
	for _, token := range tokens {
		params, err := json.Marshal(codexAgentMessageDeltaNotification{
			ThreadID: "thread-1",
			TurnID:   "turn-1",
			ItemID:   "msg-1",
			Delta:    token,
		})
		if err != nil {
			t.Fatalf("marshal delta: %v", err)
		}
		if err := mapper.HandleNotification(context.Background(), "item/agentMessage/delta", params); err != nil {
			t.Fatalf("handle delta %q: %v", token, err)
		}
	}
	params, err := json.Marshal(codexItemCompletedNotification{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		Item: codexThreadItem{
			Type: "agentMessage",
			ID:   "msg-1",
			Text: "Inspect the Rust crate first, then update the dependency and build against the new API. If it breaks I'll patch.",
		},
	})
	if err != nil {
		t.Fatalf("marshal completed item: %v", err)
	}
	if err := mapper.HandleNotification(context.Background(), "item/completed", params); err != nil {
		t.Fatalf("handle completed item: %v", err)
	}

	const expected = "Inspect the Rust crate first, then update the dependency and build against the new API. If it breaks I'll patch."
	if got := mapper.AssistantText(); got != expected {
		t.Fatalf("unexpected assistant text %q", got)
	}
	completed := false
	for _, event := range eventSink.events {
		if event.Type == "assistant_message_delta" {
			t.Fatalf("codex assistant deltas should be buffered, got %#v", event)
		}
		if event.Type == "assistant_message_completed" {
			completed = true
			if event.Data["content"] != expected {
				t.Fatalf("completed event used wrong content: %#v", event)
			}
		}
	}
	if !completed {
		t.Fatalf("expected assistant_message_completed, got %#v", eventSink.events)
	}
}

func TestCodexEventMapperRepairsAssistantTextFromCompletedItem(t *testing.T) {
	eventSink := &testEventSink{}
	mapper := newCodexEventMapper(&ExecutionContext{
		AppID:     "app-a",
		Agent:     &agentcore.Agent{Name: "Codex", RuntimeKind: agentcore.RuntimeCodex},
		Run:       &agentcore.AgentRun{ID: "run-codex-completed-text", AppID: "app-a", RuntimeKind: agentcore.RuntimeCodex},
		EventSink: eventSink,
	}, "/tmp")
	for _, token := range []string{"inspect", "the", "Rust", "crate"} {
		params, err := json.Marshal(codexAgentMessageDeltaNotification{
			ThreadID: "thread-1",
			TurnID:   "turn-1",
			ItemID:   "msg-1",
			Delta:    token,
		})
		if err != nil {
			t.Fatalf("marshal delta: %v", err)
		}
		if err := mapper.HandleNotification(context.Background(), "item/agentMessage/delta", params); err != nil {
			t.Fatalf("handle delta %q: %v", token, err)
		}
	}
	params, err := json.Marshal(codexItemCompletedNotification{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		Item: codexThreadItem{
			Type: "agentMessage",
			ID:   "msg-1",
			Text: "Inspect the Rust crate first, then build against the new API.",
		},
	})
	if err != nil {
		t.Fatalf("marshal completed item: %v", err)
	}
	if err := mapper.HandleNotification(context.Background(), "item/completed", params); err != nil {
		t.Fatalf("handle completed item: %v", err)
	}

	const expected = "Inspect the Rust crate first, then build against the new API."
	if got := mapper.AssistantText(); got != expected {
		t.Fatalf("completed item did not repair assistant text: %q", got)
	}
	for _, event := range eventSink.events {
		if event.Type == "assistant_message_completed" && event.Data["content"] != expected {
			t.Fatalf("completed event used uncorrected content: %#v", event)
		}
	}
}

func TestCodexEventMapperEmitsPreambleAndFinalAnswerAsDistinctMessages(t *testing.T) {
	eventSink := &testEventSink{}
	mapper := newCodexEventMapper(&ExecutionContext{
		AppID:     "app-a",
		Agent:     &agentcore.Agent{Name: "Codex", RuntimeKind: agentcore.RuntimeCodex},
		Run:       &agentcore.AgentRun{ID: "run-codex-multiple-messages", AppID: "app-a", RuntimeKind: agentcore.RuntimeCodex},
		EventSink: eventSink,
	}, "/tmp")

	for _, item := range []codexThreadItem{
		{Type: "agentMessage", ID: "msg-preamble", Text: "I will inspect the changelog first."},
		{Type: "agentMessage", ID: "msg-final", Text: "Here are the newest product launches."},
	} {
		params, err := json.Marshal(codexItemCompletedNotification{
			ThreadID: "thread-1",
			TurnID:   "turn-1",
			Item:     item,
		})
		if err != nil {
			t.Fatalf("marshal completed item: %v", err)
		}
		if err := mapper.HandleNotification(context.Background(), "item/completed", params); err != nil {
			t.Fatalf("handle completed item: %v", err)
		}
	}

	var completedIDs []string
	var completedContent []string
	for _, event := range eventSink.events {
		if event.Type == "assistant_message_completed" {
			completedIDs = append(completedIDs, strings.TrimSpace(event.Data["message_id"].(string)))
			completedContent = append(completedContent, strings.TrimSpace(event.Data["content"].(string)))
		}
	}
	if len(completedIDs) != 2 {
		t.Fatalf("completed assistant messages = %d, want 2: %#v", len(completedIDs), eventSink.events)
	}
	if completedContent[0] != "I will inspect the changelog first." {
		t.Fatalf("unexpected preamble content: %q", completedContent[0])
	}
	if completedContent[1] != "Here are the newest product launches." {
		t.Fatalf("unexpected final content: %q", completedContent[1])
	}
	if completedIDs[0] == completedIDs[1] {
		t.Fatalf("preamble and final answer reused a message id: %#v", completedIDs)
	}
	if got := mapper.AssistantText(); got != "Here are the newest product launches." {
		t.Fatalf("final assistant text = %q", got)
	}
	if got := mapper.AssistantMessageID(); got != completedIDs[1] {
		t.Fatalf("final assistant message id = %q, want %q", got, completedIDs[1])
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

func TestCodexRespondToPendingRequestReplayPath(t *testing.T) {
	adapter := NewCodexAdapterWithConfig(CodexConfig{PendingReplayTimeout: time.Second})
	pendingPayload := json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","command":"git status"}`)
	state := &codexSessionState{PendingRequest: &codexPendingRequest{
		Kind:         codexPendingRequestKindCommandApproval,
		RequestIDRaw: json.RawMessage(`4`),
		Payload:      pendingPayload,
	}}
	client := &fakeCodexRPC{next: []codexRPCMessage{{
		ID:     json.RawMessage(`4`),
		Method: "item/commandExecution/requestApproval",
	}}}
	resume, err := adapter.respondToPendingCodexRequest(context.Background(), client, &ExecutionContext{
		Run: &agentcore.AgentRun{Input: agentcore.RunInput{Metadata: map[string]interface{}{
			"last_resume": map[string]interface{}{"intent": "approve"},
		}}},
	}, state)
	if err != nil {
		t.Fatalf("respond to pending: %v", err)
	}
	if !resume.Replayed || strings.TrimSpace(resume.FallbackPrompt) != "" {
		t.Fatalf("expected replay path, got %#v", resume)
	}
	if len(client.responds) != 1 || !strings.Contains(client.responds[0], `"accept"`) {
		t.Fatalf("expected accept response, got %#v", client.responds)
	}
}

func TestCodexRespondToPendingRequestTimeoutBuildsFallback(t *testing.T) {
	adapter := NewCodexAdapterWithConfig(CodexConfig{PendingReplayTimeout: 10 * time.Millisecond})
	state := &codexSessionState{PendingRequest: &codexPendingRequest{
		Kind:    codexPendingRequestKindCommandApproval,
		Payload: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","command":"git status","cwd":"/tmp"}`),
	}}
	client := &fakeCodexRPC{}
	resume, err := adapter.respondToPendingCodexRequest(context.Background(), client, &ExecutionContext{
		Run: &agentcore.AgentRun{Input: agentcore.RunInput{Metadata: map[string]interface{}{
			"last_resume": map[string]interface{}{"intent": "approve"},
		}}},
	}, state)
	if err != nil {
		t.Fatalf("respond timeout fallback: %v", err)
	}
	if resume.Replayed || !strings.Contains(resume.FallbackPrompt, "git status") || !strings.Contains(resume.FallbackPrompt, "approved") {
		t.Fatalf("expected command approval fallback prompt, got %#v", resume)
	}
	if err := adapter.startCodexTurn(context.Background(), client, "thread-1", resume.FallbackPrompt); err != nil {
		t.Fatalf("start fallback turn: %v", err)
	}
	if len(client.requests) != 1 || !strings.Contains(client.requests[0], "turn/start") || !strings.Contains(client.requests[0], "git status") {
		t.Fatalf("expected fallback turn/start request, got %#v", client.requests)
	}
}

func TestCodexResumeFallbackPromptKinds(t *testing.T) {
	tests := []struct {
		name    string
		pending *codexPendingRequest
		intent  string
		content string
		want    string
	}{
		{
			name:    "file approval",
			pending: &codexPendingRequest{Kind: codexPendingRequestKindFileApproval, Payload: json.RawMessage(`{"grantRoot":"/repo","reason":"apply patch"}`)},
			intent:  "approve",
			want:    "/repo",
		},
		{
			name:    "permissions approval",
			pending: &codexPendingRequest{Kind: codexPendingRequestKindPermissions, Payload: json.RawMessage(`{"reason":"fetch dependency","permissions":{"network":{"enabled":true},"fileSystem":{"write":["/repo"]}}}`)},
			intent:  "approve",
			want:    "Network access granted: true",
		},
		{
			name:    "request changes",
			pending: &codexPendingRequest{Kind: codexPendingRequestKindCommandApproval, Payload: json.RawMessage(`{"command":"git status"}`)},
			intent:  "request_changes",
			content: "Use a read-only command.",
			want:    "Use a read-only command.",
		},
		{
			name:    "human input",
			pending: &codexPendingRequest{Kind: codexPendingRequestKindHumanInput},
			intent:  "reply",
			content: "Pick option A.",
			want:    "Pick option A.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := codexResumeFallbackPrompt(tt.pending, tt.intent, tt.content, nil)
			if err != nil {
				t.Fatalf("fallback prompt: %v", err)
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("expected %q in fallback prompt, got %q", tt.want, got)
			}
		})
	}
}

type fakeCodexRPC struct {
	next     []codexRPCMessage
	responds []string
	requests []string
}

func (f *fakeCodexRPC) Next(ctx context.Context) (codexRPCMessage, error) {
	if len(f.next) > 0 {
		msg := f.next[0]
		f.next = f.next[1:]
		return msg, nil
	}
	<-ctx.Done()
	return codexRPCMessage{}, ctx.Err()
}

func (f *fakeCodexRPC) Respond(_ context.Context, _ json.RawMessage, result any) error {
	payload, _ := json.Marshal(result)
	f.responds = append(f.responds, string(payload))
	return nil
}

func (f *fakeCodexRPC) Request(_ context.Context, method string, params any) (json.RawMessage, error) {
	payload, _ := json.Marshal(params)
	f.requests = append(f.requests, method+":"+string(payload))
	return json.RawMessage(`{"turn":{"id":"turn-test","status":"running"}}`), nil
}

func TestCodexAdapterDeclinesGitPushApprovalAndContinues(t *testing.T) {
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
printf '%s\n' '{"id":4,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","command":"git push -u origin feature","cwd":"/tmp"}}'
IFS= read -r line
case "$line" in
  *'"decision":"decline"'*) ;;
  *) exit 2 ;;
esac
IFS= read -r line
case "$line" in
  *'"method":"turn/start"'*) ;;
  *) exit 3 ;;
esac
printf '%s\n' '{"id":4,"result":{"turn":{"id":"turn-2","status":"running"}}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-2","itemId":"msg-1","delta":"kept local only"}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-2","status":"completed"}}}'
sleep 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	adapter := NewCodexAdapterWithConfig(CodexConfig{
		CommandPath: command,
		WorkDir:     tmp,
		Timeout:     2 * time.Second,
		AppServer:   true,
	})
	run := &agentcore.AgentRun{
		ID:          "run-no-push",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Instructions: "change code"},
		RuntimeKind: agentcore.RuntimeCodex,
	}
	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "Codex", Provider: "openai", Model: "gpt"},
		Run:     run,
		InteractionBroker: testInteractionBroker{
			store: mem,
			run:   run,
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.WaitForApproval {
		t.Fatalf("git push approval should be declined internally, got approval result %#v", result)
	}
	if result.AssistantMessage != "kept local only" {
		t.Fatalf("expected continued assistant message, got %q", result.AssistantMessage)
	}
	interactions, err := mem.ListInteractions(context.Background(), "app-a", "run-no-push")
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 0 {
		t.Fatalf("forbidden git push should not surface a user approval interaction, got %#v", interactions)
	}
}

func TestInstallCodexCommandGuardsBlocksGitPush(t *testing.T) {
	runRoot := t.TempDir()
	guardDir, err := installCodexCommandGuards(runRoot)
	if err != nil {
		t.Fatalf("install command guards: %v", err)
	}

	cmd := exec.Command(filepath.Join(guardDir, "git"), "push", "origin", "main")
	cmd.Dir = runRoot
	cmd.Env = append([]string{}, "PATH="+guardDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected git push to be blocked")
	}
	if !strings.Contains(string(output), codexDeliveryGuardMessage) {
		t.Fatalf("expected guard message in output, got %q", string(output))
	}
}

func TestInstallCodexCommandGuardsBlocksGitPushWithCFlag(t *testing.T) {
	runRoot := t.TempDir()
	guardDir, err := installCodexCommandGuards(runRoot)
	if err != nil {
		t.Fatalf("install command guards: %v", err)
	}

	cmd := exec.Command(filepath.Join(guardDir, "git"), "-C", runRoot, "push", "origin", "main")
	cmd.Dir = runRoot
	cmd.Env = append([]string{}, "PATH="+guardDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected git -C ... push to be blocked")
	}
	if !strings.Contains(string(output), codexDeliveryGuardMessage) {
		t.Fatalf("expected guard message in output, got %q", string(output))
	}
}

func TestInstallCodexCommandGuardsAllowsLocalGitCommands(t *testing.T) {
	runRoot := t.TempDir()
	guardDir, err := installCodexCommandGuards(runRoot)
	if err != nil {
		t.Fatalf("install command guards: %v", err)
	}

	cmd := exec.Command(filepath.Join(guardDir, "git"), "--version")
	cmd.Dir = runRoot
	cmd.Env = append([]string{}, "PATH="+guardDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected git --version to pass through, got err=%v output=%q", err, string(output))
	}
	if !strings.Contains(string(output), "git version") {
		t.Fatalf("expected git --version output, got %q", string(output))
	}
}

func TestInstallCodexCommandGuardsBlocksGHPRCreateWithRepoFlag(t *testing.T) {
	runRoot := t.TempDir()
	guardDir, err := installCodexCommandGuards(runRoot)
	if err != nil {
		t.Fatalf("install command guards: %v", err)
	}
	guardPath := filepath.Join(guardDir, "gh")
	if _, err := os.Stat(guardPath); err != nil {
		if os.IsNotExist(err) {
			t.Skip("gh is not installed in test environment")
		}
		t.Fatalf("stat gh guard: %v", err)
	}

	cmd := exec.Command(guardPath, "-R", "owner/repo", "pr", "create", "--title", "Test PR")
	cmd.Dir = runRoot
	cmd.Env = append([]string{}, "PATH="+guardDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected gh -R ... pr create to be blocked")
	}
	if !strings.Contains(string(output), codexDeliveryGuardMessage) {
		t.Fatalf("expected guard message in output, got %q", string(output))
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
