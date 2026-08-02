package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocol "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

func TestOpenCodeResolveModelIDDefaultsToAnthropic(t *testing.T) {
	adapter := NewOpenCodeAdapterWithConfig(OpenCodeConfig{CommandPath: "opencode"})

	got := adapter.resolveModelID(&agentcore.Agent{})

	if got != "anthropic/"+defaultNativeAnthropicModel {
		t.Fatalf("expected anthropic default model, got %q", got)
	}
}

func TestBuildOpenCodeConfigContentIncludesStagedSkillPath(t *testing.T) {
	payload, err := buildOpenCodeConfigContent(&ExecutionContext{
		Agent:           &agentcore.Agent{Name: "OpenCode", Provider: "openai", Model: "openai/gpt-5-mini"},
		Run:             &agentcore.AgentRun{Target: agentcore.TargetRef{Type: "repository", ID: "repo-1"}},
		StagedSkillRoot: "/tmp/agent-runtime-skills/run-123",
	}, "openai/gpt-5-mini", "system prompt", map[string]any{
		"openai": map[string]any{"models": map[string]any{"gpt-5-mini": map[string]any{}}},
	})
	if err != nil {
		t.Fatalf("build opencode config: %v", err)
	}
	body := decodeOpenCodeConfig(payload)
	if body["$schema"] != "https://opencode.ai/config.json" {
		t.Fatalf("unexpected schema: %#v", body["$schema"])
	}
	skills, _ := body["skills"].(map[string]any)
	paths, _ := skills["paths"].([]any)
	if len(paths) != 1 || paths[0] != "/tmp/agent-runtime-skills/run-123" {
		t.Fatalf("expected staged skill path in config, got %s", payload)
	}
	if !strings.Contains(payload, "system prompt") {
		t.Fatalf("expected system prompt in config, got %s", payload)
	}
}

func TestOpenCodeBuildEnvUsesIsolatedHomeForRun(t *testing.T) {
	tmp := t.TempDir()
	adapter := NewOpenCodeAdapterWithConfig(OpenCodeConfig{
		CommandPath:       "opencode",
		RuntimeRoot:       filepath.Join(tmp, "runtime"),
		AnthropicAPIKey:   "anthropic-key",
		AnthropicBaseURL:  "https://anthropic.example/v1",
		OpenRouterAPIKey:  "openrouter-key",
		OpenRouterBaseURL: "https://openrouter.example/api/v1",
	})

	env, err := adapter.buildEnv(&ExecutionContext{
		Agent: &agentcore.Agent{Provider: "anthropic"},
		Run:   &agentcore.AgentRun{ID: "run-123", AppID: "app-a"},
	}, `{"agent":{}}`)
	if err != nil {
		t.Fatalf("build env: %v", err)
	}
	lookup := envMap(env)
	expectedHome := filepath.Join(tmp, "runtime", "app-a", "run-123", "home")
	if lookup["HOME"] != expectedHome {
		t.Fatalf("expected isolated HOME %q, got %q", expectedHome, lookup["HOME"])
	}
	if lookup["OPENCODE_HOME"] != filepath.Join(expectedHome, ".opencode") {
		t.Fatalf("expected isolated OPENCODE_HOME, got %q", lookup["OPENCODE_HOME"])
	}
	if lookup["ANTHROPIC_API_KEY"] != "anthropic-key" {
		t.Fatalf("expected anthropic key in env")
	}
	if lookup["OPENCODE_CONFIG_CONTENT"] != `{"agent":{}}` {
		t.Fatalf("expected config content env")
	}
}

func TestOpenCodeConfigUsesLoopbackMCPBrokerWithoutEmbeddingToken(t *testing.T) {
	execCtx := &ExecutionContext{
		Agent:          &agentcore.Agent{Name: "OpenCode"},
		Run:            &agentcore.AgentRun{ID: "run-1", AppID: "app-1", Target: agentcore.TargetRef{Type: "workspace", ID: "ws-1"}},
		MCPBrokerURL:   "http://127.0.0.1:43210",
		MCPBrokerToken: "broker-secret",
	}
	payload, err := buildOpenCodeConfigContent(execCtx, "", "system", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"agent_runtime"`) || !strings.Contains(payload, `{env:AGENT_RUNTIME_RUN_MCP_TOKEN}`) {
		t.Fatalf("expected run MCP broker config, got %s", payload)
	}
	if strings.Contains(payload, "broker-secret") {
		t.Fatalf("broker token leaked into OpenCode config: %s", payload)
	}
	env, err := NewOpenCodeAdapterWithConfig(OpenCodeConfig{CommandPath: "opencode", RuntimeRoot: t.TempDir()}).buildEnv(execCtx, payload)
	if err != nil {
		t.Fatal(err)
	}
	if envMap(env)["AGENT_RUNTIME_RUN_MCP_TOKEN"] != "broker-secret" {
		t.Fatal("broker token was not passed through the isolated process environment")
	}
}

func TestOpenCodeMCPBrokerAuthenticatesAndExposesOnlyRunMCPTools(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := &agentcore.Agent{ID: "agent-1", AppID: "app-1", Name: "OpenCode", RuntimeKind: agentcore.RuntimeOpenCode, ApprovalMode: agentcore.ApprovalModeNever}
	if err := mem.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{ID: "run-1", AppID: "app-1", AgentID: agent.ID, Target: agentcore.TargetRef{Type: "workspace", ID: "ws-1"}, Status: agentcore.RunStatusRunning}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	registry.Register(tools.Definition{Name: "builtin_read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"builtin":true}`), nil
	})
	registry.Register(tools.Definition{Name: "mcp__github__get_issue", InputSchema: map[string]any{"type": "object"}}, func(context.Context, tools.CallContext, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"issue":"42"}`), nil
	})
	broker, err := startOpenCodeMCPBroker(ctx, &ExecutionContext{
		AppID: "app-1", Agent: agent, Run: run, Store: mem, Tools: registry,
		AllowedTools: map[string]bool{"builtin_read": true, "mcp__github__get_issue": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if broker == nil {
		t.Fatal("expected OpenCode MCP broker")
	}
	defer broker.Close()

	resp, err := http.Get(broker.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated broker status = %d", resp.StatusCode)
	}

	httpClient := &http.Client{Transport: bearerRoundTripper{base: http.DefaultTransport, token: broker.Token}}
	client := protocol.NewClient(&protocol.Implementation{Name: "opencode-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &protocol.StreamableClientTransport{Endpoint: broker.URL, HTTPClient: httpClient, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var names []string
	for tool, listErr := range session.Tools(ctx, nil) {
		if listErr != nil {
			t.Fatal(listErr)
		}
		names = append(names, tool.Name)
	}
	if len(names) != 1 || names[0] != "mcp__github__get_issue" {
		t.Fatalf("broker tools = %#v", names)
	}
	result, err := session.CallTool(ctx, &protocol.CallToolParams{Name: names[0], Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("call broker tool: result=%#v err=%v", result, err)
	}
	calls, err := mem.ListToolCalls(ctx, "app-1", run.ID)
	if err != nil || len(calls) != 1 || calls[0].ToolName != names[0] {
		t.Fatalf("audited calls=%#v err=%v", calls, err)
	}
}

type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (t bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

func TestOpenCodeAdapterExecutesConfiguredCommand(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "opencode")
	script := `#!/bin/sh
if [ "$1" != "run" ]; then
  echo "unexpected command" >&2
  exit 1
fi
printf '%s\n' '{"type":"text","part":{"id":"msg-1","text":"hello"}}'
printf '%s\n' '{"type":"step_finish","part":{"tokens":{"input":2,"output":3,"reasoning":1,"cache":{"read":4}}}}'
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-opencode",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Instructions: "change code"},
		RuntimeKind: agentcore.RuntimeOpenCode,
	}
	adapter := NewOpenCodeAdapterWithConfig(OpenCodeConfig{
		CommandPath: command,
		WorkDir:     tmp,
		RuntimeRoot: filepath.Join(tmp, "runtime"),
		Timeout:     5 * time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "OpenCode", Provider: "anthropic"},
		Run:     run,
		ArtifactWriter: testArtifactWriter{
			store: mem,
			run:   run,
		},
		EventSink: &testEventSink{},
	})
	if err != nil {
		t.Fatalf("execute opencode: %v", err)
	}
	if result.AssistantMessage != "hello" {
		t.Fatalf("unexpected assistant message: %q", result.AssistantMessage)
	}
	for _, expected := range []string{`"runtime_kind":"opencode"`, `"input_tokens":2`, `"output_tokens":3`, `"reasoning_output_tokens":1`, `"cached_input_tokens":4`} {
		if !strings.Contains(string(result.OutputSummary), expected) {
			t.Fatalf("expected %s in output summary, got %s", expected, string(result.OutputSummary))
		}
	}
	artifacts, err := mem.ListArtifacts(context.Background(), "app-a", "run-opencode")
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	wantArtifacts := map[string]bool{
		"opencode_config": false,
		"opencode_prompt": false,
		"opencode_stdout": false,
		"agent_summary":   false,
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
}

func TestOpenCodeAdapterWithoutWorkspaceLeaseUsesIsolatedWorkDir(t *testing.T) {
	current, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	tmp := t.TempDir()
	command := filepath.Join(tmp, "opencode")
	script := `#!/bin/sh
if [ "$1" != "run" ]; then
  echo "unexpected command" >&2
  exit 1
fi
printf '{"type":"text","part":{"id":"msg-1","text":"%s"}}\n' "$PWD"
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-no-lease",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "workspace", ID: "ws-1"},
		Input:       agentcore.RunInput{Instructions: "inspect context"},
		RuntimeKind: agentcore.RuntimeOpenCode,
	}
	adapter := NewOpenCodeAdapterWithConfig(OpenCodeConfig{
		CommandPath: command,
		RuntimeRoot: filepath.Join(tmp, "runtime"),
		Timeout:     5 * time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "OpenCode", Provider: "anthropic"},
		Run:     run,
		ArtifactWriter: testArtifactWriter{
			store: mem,
			run:   run,
		},
		EventSink: &testEventSink{},
	})
	if err != nil {
		t.Fatalf("execute opencode: %v", err)
	}
	if result.AssistantMessage == current {
		t.Fatalf("opencode used runtime process cwd as workdir: %q", result.AssistantMessage)
	}
	if !strings.Contains(result.AssistantMessage, "agent-runtime-opencode-run-no-lease") {
		t.Fatalf("expected isolated opencode workdir, got %q", result.AssistantMessage)
	}
}

func TestOpenCodeAdapterMasksRepoSkillRootsDuringRun(t *testing.T) {
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
	command := filepath.Join(tmp, "opencode")
	script := `#!/bin/sh
if [ ! -e ".agents/skills" ] && [ -d ".agents/.agent-runtime-hidden-skills-run-mask-opencode" ] && [ ! -e ".codex/skills" ] && [ -d ".codex/.agent-runtime-hidden-skills-run-mask-opencode" ]; then status="masked"; else status="visible"; fi
printf '%s\n' "{\"type\":\"text\",\"part\":{\"id\":\"msg-1\",\"text\":\"$status\"}}"
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	adapter := NewOpenCodeAdapterWithConfig(OpenCodeConfig{
		CommandPath: command,
		WorkDir:     repo,
		RuntimeRoot: filepath.Join(tmp, "runtime"),
		Timeout:     5 * time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Agent:   &agentcore.Agent{Name: "OpenCode", Provider: "anthropic"},
		Run: &agentcore.AgentRun{
			ID:          "run-mask-opencode",
			AppID:       "app-a",
			Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
			Input:       agentcore.RunInput{Instructions: "change code"},
			RuntimeKind: agentcore.RuntimeOpenCode,
		},
	})
	if err != nil {
		t.Fatalf("execute opencode: %v", err)
	}
	if result.AssistantMessage != "masked" {
		t.Fatalf("expected repo skill roots to be masked during opencode run, got %q", result.AssistantMessage)
	}
	if _, err := os.Stat(filepath.Join(repo, ".agents", "skills", "repo-agent", "SKILL.md")); err != nil {
		t.Fatalf("expected .agents skills to be restored, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".codex", "skills", "repo-codex", "SKILL.md")); err != nil {
		t.Fatalf("expected .codex skills to be restored, stat err=%v", err)
	}
}

func TestOpenCodeAdapterCreatesInputInteractionFromFencedHandoff(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "opencode")
	handoffText := "Need a choice.\n\n```agent-runtime-input\n{\"intent\":\"request_user_input\",\"title\":\"Choose path\",\"summary\":\"Pick one option\",\"options\":[{\"label\":\"A\"}]}\n```"
	event, _ := json.Marshal(map[string]any{
		"type": "text",
		"part": map[string]any{"id": "msg-1", "text": handoffText},
	})
	script := "#!/bin/sh\nprintf '%s\\n' '" + string(event) + "'\n"
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-input",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Instructions: "change code"},
		RuntimeKind: agentcore.RuntimeOpenCode,
	}
	adapter := NewOpenCodeAdapterWithConfig(OpenCodeConfig{
		CommandPath: command,
		WorkDir:     tmp,
		RuntimeRoot: filepath.Join(tmp, "runtime"),
		Timeout:     5 * time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "OpenCode", Provider: "anthropic"},
		Run:     run,
		ArtifactWriter: testArtifactWriter{
			store: mem,
			run:   run,
		},
	})
	if err != nil {
		t.Fatalf("execute opencode: %v", err)
	}
	if !result.AwaitingInput || result.WaitForApproval {
		t.Fatalf("expected input pause result, got %#v", result)
	}
	interactions, err := mem.ListInteractions(context.Background(), "app-a", "run-input")
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].InteractionKind != "request_user_input" || interactions[0].Status != "pending" {
		t.Fatalf("expected pending input interaction, got %#v", interactions)
	}
	if !strings.Contains(string(interactions[0].RequestPayload), "Pick one option") {
		t.Fatalf("expected handoff payload in interaction, got %s", string(interactions[0].RequestPayload))
	}
}

func TestOpenCodeAdapterMapsReasoningActivityAndToolEvents(t *testing.T) {
	tmp := t.TempDir()
	command := filepath.Join(tmp, "opencode")
	events := []string{
		`{"type":"message.part.updated","part":{"id":"reason-1","type":"reasoning","text":"Inspecting file","time":{"end":1}}}`,
		`{"type":"step_start","part":{"stepID":"step-1","title":"Inspect"}}`,
		`{"type":"message.part.updated","part":{"id":"tool-part-1","type":"tool","callID":"call-1","messageID":"msg-1","state":{"status":"running","title":"Bash","input":{"command":"go test"},"time":{"start":100}}}}`,
		`{"type":"message.part.delta","properties":{"partID":"tool-part-1","field":"state.input.command","delta":" ./..."}}`,
		`{"type":"message.part.updated","part":{"id":"tool-part-1","type":"tool","callID":"call-1","messageID":"msg-1","state":{"status":"completed","title":"Bash","input":{"command":"go test ./..."},"output":"ok","time":{"start":100,"end":145}}}}`,
		`{"type":"message.part.updated","part":{"id":"msg-1","type":"text","messageID":"msg-1","text":"done","time":{"end":2}}}`,
		`{"type":"step_finish","part":{"stepID":"step-1","stopReason":"end_turn","tokens":{"input":2,"output":3}}}`,
	}
	if err := os.WriteFile(command, []byte(openCodeFixtureScript(events...)), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-events",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Input:       agentcore.RunInput{Instructions: "inspect"},
		RuntimeKind: agentcore.RuntimeOpenCode,
	}
	eventSink := &testEventSink{}
	adapter := NewOpenCodeAdapterWithConfig(OpenCodeConfig{
		CommandPath: command,
		WorkDir:     tmp,
		RuntimeRoot: filepath.Join(tmp, "runtime"),
		Timeout:     5 * time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "OpenCode", Provider: "anthropic"},
		Run:     run,
		ArtifactWriter: testArtifactWriter{
			store: mem,
			run:   run,
		},
		EventSink: eventSink,
	})
	if err != nil {
		t.Fatalf("execute opencode: %v", err)
	}
	if result.AssistantMessage != "done" {
		t.Fatalf("unexpected assistant message: %q", result.AssistantMessage)
	}
	for _, eventType := range []string{
		"reasoning_message_started",
		"reasoning_message_delta",
		"reasoning_message_completed",
		"activity_snapshot",
		"activity_delta",
		"tool_call_started",
		"tool_call_args_delta",
		"tool_call_result",
		"tool_call_finished",
	} {
		if !eventSink.hasType(eventType) {
			t.Fatalf("expected event %s, got %#v", eventType, eventSink.events)
		}
	}
	toolCalls, err := mem.ListToolCalls(context.Background(), "app-a", "run-events")
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected one durable tool call, got %#v", toolCalls)
	}
	if toolCalls[0].ToolName != "Bash" || !strings.Contains(string(toolCalls[0].Input), `"command":"go test ./..."`) || !strings.Contains(string(toolCalls[0].Output), `"summary":"ok"`) {
		t.Fatalf("unexpected durable tool call: input=%s output=%s call=%#v", string(toolCalls[0].Input), string(toolCalls[0].Output), toolCalls[0])
	}
	if !strings.Contains(string(result.OutputSummary), `"duration_ms":45`) {
		t.Fatalf("expected tool duration summary, got %s", string(result.OutputSummary))
	}
}

func TestOpenCodeAdapterPersistsHostPreparedRepositoryChanges(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	initTestGitRepo(t, repo)
	command := filepath.Join(tmp, "opencode")
	script := `#!/bin/sh
printf '%s\n' "updated" > feature.txt
printf '%s\n' '{"type":"text","part":{"id":"msg-1","text":"implemented"}}'
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-commit",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Instructions: "change code", Metadata: map[string]interface{}{"commit_message": "agent-runtime: test change"}},
		RuntimeKind: agentcore.RuntimeOpenCode,
	}
	adapter := NewOpenCodeAdapterWithConfig(OpenCodeConfig{
		CommandPath: command,
		WorkDir:     repo,
		RuntimeRoot: filepath.Join(tmp, "runtime"),
		Timeout:     5 * time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context:        context.Background(),
		AppID:          "app-a",
		Store:          mem,
		Agent:          &agentcore.Agent{Name: "OpenCode", Provider: "anthropic"},
		Run:            run,
		WorkspaceLease: &agentcore.WorkspaceLease{ID: "lease-1", Provider: "host_app", RootPath: repo, Metadata: map[string]interface{}{"work_branch": "main"}},
		ArtifactWriter: testArtifactWriter{
			store: mem,
			run:   run,
		},
	})
	if err != nil {
		t.Fatalf("execute opencode: %v", err)
	}
	if !strings.Contains(string(result.OutputSummary), `"commit_message":"agent-runtime: test change"`) {
		t.Fatalf("expected commit metadata in output summary, got %s", string(result.OutputSummary))
	}
	if got := runTestGit(t, repo, "log", "-1", "--pretty=%s"); strings.TrimSpace(got) != "agent-runtime: test change" {
		t.Fatalf("expected local commit, got %q", got)
	}
	artifacts, err := mem.ListArtifacts(context.Background(), "app-a", "run-commit")
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	wantArtifacts := map[string]bool{"diff": false, "file_bundle": false, "git_persistence_result": false}
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
}

func TestOpenCodeAdapterLeavesRepositoryProviderCommitToWorkspaceFinalizer(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	initTestGitRepo(t, repo)
	command := filepath.Join(tmp, "opencode")
	script := `#!/bin/sh
printf '%s\n' "updated" > feature.txt
printf '%s\n' '{"type":"text","part":{"id":"msg-1","text":"implemented"}}'
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID:          "run-stage",
		AppID:       "app-a",
		Target:      agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		Input:       agentcore.RunInput{Instructions: "change code"},
		RuntimeKind: agentcore.RuntimeOpenCode,
	}
	adapter := NewOpenCodeAdapterWithConfig(OpenCodeConfig{
		CommandPath: command,
		WorkDir:     repo,
		RuntimeRoot: filepath.Join(tmp, "runtime"),
		Timeout:     5 * time.Second,
	})

	result, err := adapter.Execute(&ExecutionContext{
		Context: context.Background(),
		AppID:   "app-a",
		Store:   mem,
		Agent:   &agentcore.Agent{Name: "OpenCode", Provider: "anthropic"},
		Run:     run,
		WorkspaceLease: &agentcore.WorkspaceLease{
			ID:       "lease-1",
			Provider: "repository",
			RootPath: repo,
			Metadata: map[string]interface{}{
				"work_branch": "main",
				"repository_spec": map[string]interface{}{
					"clone_url":       "https://example.invalid/repo.git",
					"finalize_policy": "local_commit",
				},
			},
		},
		ArtifactWriter: testArtifactWriter{
			store: mem,
			run:   run,
		},
	})
	if err != nil {
		t.Fatalf("execute opencode: %v", err)
	}
	if strings.Contains(string(result.OutputSummary), `"commit_sha"`) {
		t.Fatalf("did not expect adapter commit for repository provider, got %s", string(result.OutputSummary))
	}
	if !strings.Contains(string(result.OutputSummary), `"finalize_policy":"workspace_local_commit"`) {
		t.Fatalf("expected workspace finalizer marker, got %s", string(result.OutputSummary))
	}
	if got := runTestGit(t, repo, "log", "--oneline"); strings.Count(strings.TrimSpace(got), "\n")+1 != 1 {
		t.Fatalf("expected only seed commit before workspace finalizer, got %q", got)
	}
	if diff := runTestGit(t, repo, "diff", "--cached", "--name-only"); strings.TrimSpace(diff) != "feature.txt" {
		t.Fatalf("expected staged feature file for workspace finalizer, got %q", diff)
	}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func TestParseOpenCodeJSONEventTextAndTokens(t *testing.T) {
	parsed, err := parseOpenCodeJSONEvent(`{"type":"text","part":{"id":"msg_1","text":"{\"status\":\"success\"}"}}`)
	if err != nil {
		t.Fatalf("parse text event: %v", err)
	}
	if parsed.ResponseText != `{"status":"success"}` {
		t.Fatalf("unexpected response text: %q", parsed.ResponseText)
	}

	parsed, err = parseOpenCodeJSONEvent(`{"type":"step_finish","part":{"tokens":{"input":120,"output":45,"reasoning":10,"cache":{"read":8,"write":3}}}}`)
	if err != nil {
		t.Fatalf("parse step finish: %v", err)
	}
	if parsed.TokensUsed != 186 {
		body, _ := json.Marshal(parsed)
		t.Fatalf("expected 186 tokens, got %d in %s", parsed.TokensUsed, string(body))
	}
}

func initTestGitRepo(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runTestGit(t, repo, "init", "-b", "main")
	runTestGit(t, repo, "config", "user.name", "Agent Runtime Test")
	runTestGit(t, repo, "config", "user.email", "agent-runtime-test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runTestGit(t, repo, "add", "README.md")
	runTestGit(t, repo, "commit", "-m", "seed")
}

func runTestGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(output))
	}
	return string(output)
}

func openCodeFixtureScript(lines ...string) string {
	var builder strings.Builder
	builder.WriteString("#!/bin/sh\n")
	builder.WriteString("cat <<'AGENT_RUNTIME_OPENCODE_FIXTURE'\n")
	builder.WriteString(strings.Join(lines, "\n"))
	builder.WriteString("\nAGENT_RUNTIME_OPENCODE_FIXTURE\n")
	return builder.String()
}
