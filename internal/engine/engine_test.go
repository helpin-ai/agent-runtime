package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/skills"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestStartRunCompletesLightweightNativeRun(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	targets := host.NewStaticContextProvider()
	targets.Register("app-a", agentcore.TargetRef{Type: "ticket", ID: "T-1"}, host.TargetContext{Summary: "ticket context"})
	eng := testEngine(mem, targets)

	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Instructions: "summarize",
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}

	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusCompleted)
	if run.OutputSummary == nil {
		t.Fatal("expected output summary")
	}
	messages, err := mem.ListMessages(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Role != "assistant" {
		t.Fatalf("expected assistant message, got %#v", messages)
	}
}

func TestStartRunEnforcesAppIsolation(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	eng := testEngine(mem, host.NewStaticContextProvider())

	_, err := eng.StartRun(ctx, StartRunRequest{
		AppID:   "app-b",
		AgentID: agent.ID,
		Target:  agentcore.TargetRef{Type: "ticket", ID: "T-1"},
	})
	if err == nil {
		t.Fatal("expected app-scoped agent lookup to fail")
	}
}

func TestStartRunRejectsDisallowedToolSubset(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.AllowedTools = []string{"get_context"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	eng := testEngine(mem, host.NewStaticContextProvider())

	_, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		AllowedTools: []string{"delete_customer"},
	})
	if err == nil {
		t.Fatal("expected disallowed tool error")
	}
}

func TestApprovalModePausesAndApproveResumes(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.ApprovalMode = agentcore.ApprovalModeAlways
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	eng := testEngine(mem, host.NewStaticContextProvider())

	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID:   "app-a",
		AgentID: agent.ID,
		Target:  agentcore.TargetRef{Type: "ticket", ID: "T-1"},
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusPaused)
	if run.PauseReason != agentcore.PauseReasonHumanApproval {
		t.Fatalf("pause_reason = %q", run.PauseReason)
	}

	if _, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{Intent: "approve"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusCompleted)
	if run.ApprovalState != agentcore.ApprovalApproved {
		t.Fatalf("approval_state = %q", run.ApprovalState)
	}
}

func TestResumeRunResolvesPendingInteractionAndKeepsPausedOutputSummary(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	adapter := &pausingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
	})

	run, err := eng.StartRun(ctx, StartRunRequest{
		AppID:        "app-a",
		AgentID:      agent.ID,
		Target:       agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		Instructions: "ask",
	})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusPaused)
	if !strings.Contains(string(run.OutputSummary), `"native_messages"`) {
		t.Fatalf("expected paused output summary to be persisted, got %s", string(run.OutputSummary))
	}
	interactions, err := mem.ListInteractions(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list interactions: %v", err)
	}
	if len(interactions) != 1 || interactions[0].Status != "pending" {
		t.Fatalf("expected pending interaction, got %#v", interactions)
	}

	if _, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{
		Intent:          "reply",
		Content:         "Sales owns it.",
		ResponsePayload: json.RawMessage(`{"answers":{"owner":{"answers":["Sales"]}}}`),
		ExternalActorID: "user-1",
	}); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	run = waitForRunStatus(t, mem, "app-a", run.ID, agentcore.RunStatusCompleted)
	if adapter.calls != 2 {
		t.Fatalf("expected adapter to execute twice, got %d", adapter.calls)
	}
	interactions, err = mem.ListInteractions(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list interactions after resume: %v", err)
	}
	if len(interactions) != 1 || interactions[0].Status != "resolved" || interactions[0].ResolvedByExternalID != "user-1" {
		t.Fatalf("expected resolved interaction, got %#v", interactions)
	}
	if string(interactions[0].ResponsePayload) != `{"answers":{"owner":{"answers":["Sales"]}}}` {
		t.Fatalf("unexpected response payload: %s", string(interactions[0].ResponsePayload))
	}
	if run.Input.Metadata == nil || run.Input.Metadata["last_resume"] == nil {
		t.Fatalf("expected last_resume metadata, got %#v", run.Input.Metadata)
	}
}

func TestExecuteRunOnceResolvesSkillInstructionsAndPolicy(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.Skills = []agentcore.SkillRef{{Key: "approval_protocol"}}
	agent.AllowedTools = []string{"request_approval"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "summarize"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adapter := &recordingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills: skills.NewRegistry(skills.Definition{
			Key:               "approval_protocol",
			Description:       "Approval protocol",
			Instructions:      "Always request approval.",
			RequiredTools:     []string{"request_approval"},
			SupportedRuntimes: []string{agentcore.RuntimeNativeSDK},
			Policy: skills.Policy{
				CompletionRequiresInteractionKinds: []string{skills.InteractionKindApprovalRequest},
			},
		}),
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if adapter.skillInstructions != "Always request approval." {
		t.Fatalf("expected skill instructions in execution context, got %q", adapter.skillInstructions)
	}
	if len(adapter.skillRefs) != 1 || adapter.skillRefs[0].Key != "approval_protocol" {
		t.Fatalf("unexpected skill refs: %#v", adapter.skillRefs)
	}
	if got := adapter.skillPolicy.CompletionRequiresInteractionKinds; len(got) != 1 || got[0] != skills.InteractionKindApprovalRequest {
		t.Fatalf("unexpected skill policy: %#v", adapter.skillPolicy)
	}
}

func TestExecuteRunOnceRejectsSkillRequiredToolMissing(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.Skills = []agentcore.SkillRef{{Key: "approval_protocol"}}
	agent.AllowedTools = []string{"get_context"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "summarize"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adapter := &recordingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills: skills.NewRegistry(skills.Definition{
			Key:           "approval_protocol",
			Description:   "Approval protocol",
			Instructions:  "Always request approval.",
			RequiredTools: []string{"request_approval"},
		}),
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err == nil {
		t.Fatal("expected missing required tool error")
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter executed despite invalid skill requirements")
	}
}

func TestExecuteRunOnceSelectsActiveNativeSkillsBeforeToolValidation(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.ExecutionConfig = json.RawMessage(`{"preset_key":"epic_planner"}`)
	agent.Skills = []agentcore.SkillRef{
		{Key: "approval_protocol"},
		{Key: "prd_authorship"},
		{Key: "task_decomposition"},
		{Key: "epic_state_routing"},
	}
	agent.AllowedTools = []string{"publish_prd_draft"}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "epic", ID: "E-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input: agentcore.RunInput{
			Instructions: "plan",
			Metadata:     map[string]interface{}{"planning_stage": skills.PlanningStageDraftSpec},
		},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adapter := &recordingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills: skills.NewRegistry(
			skills.Definition{Key: "approval_protocol", SourceKind: skills.SourceBuiltIn, Instructions: "approval"},
			skills.Definition{Key: "prd_authorship", SourceKind: skills.SourceBuiltIn, Instructions: "prd", RequiredTools: []string{"publish_prd_draft"}},
			skills.Definition{Key: "task_decomposition", SourceKind: skills.SourceBuiltIn, Instructions: "tasks", RequiredTools: []string{"publish_task_plan"}},
			skills.Definition{Key: "epic_state_routing", SourceKind: skills.SourceBuiltIn, Instructions: "routing"},
		),
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if got := skillKeys(adapter.skillRefs); strings.Join(got, ",") != "approval_protocol,prd_authorship,epic_state_routing" {
		t.Fatalf("unexpected active skill refs %#v", got)
	}
	if strings.Contains(adapter.skillInstructions, "tasks") {
		t.Fatalf("inactive task skill instructions leaked: %q", adapter.skillInstructions)
	}
}

func TestExecuteRunOnceStagesSkillsIntoWorkspace(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.Skills = []agentcore.SkillRef{{Key: "code_builder"}}
	agent.ExecutionConfig = json.RawMessage(`{"workspace":{"mode":"host_prepared"}}`)
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		RuntimeKind:   agentcore.RuntimeCodex,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "build"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{lease: agentcore.WorkspaceLease{
		ID:            "lease-1",
		Provider:      "test",
		RootPath:      t.TempDir(),
		CleanupPolicy: workspace.CleanupManual,
	}}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	adapter := &recordingRuntimeAdapter{kind: agentcore.RuntimeCodex}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills:               skills.NewDefaultRegistry(),
		Workspaces:           workspaces,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if adapter.stagedSkillRoot == "" {
		t.Fatal("expected staged skill root")
	}
	if _, err := os.Stat(filepath.Join(adapter.stagedSkillRoot, "01-code_builder", "SKILL.md")); err != nil {
		t.Fatalf("expected staged code_builder skill: %v", err)
	}
	artifacts, err := mem.ListArtifacts(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	foundManifest := false
	for _, artifact := range artifacts {
		if artifact.ArtifactType == "runtime_skill_manifest" {
			foundManifest = true
		}
	}
	if !foundManifest {
		t.Fatalf("expected runtime skill manifest artifact, got %#v", artifacts)
	}
}

func TestExecuteRunOnceStagesSkillsWithoutWorkspaceLease(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.Skills = []agentcore.SkillRef{{Key: "code_builder"}}
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "message_generation_task", ID: "task-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "answer"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adapter := &recordingRuntimeAdapter{}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Skills:               skills.NewDefaultRegistry(),
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if adapter.stagedSkillRoot == "" {
		t.Fatal("expected staged skill root")
	}
	if !strings.Contains(adapter.stagedSkillRoot, filepath.Join("agent-runtime-skills", run.ID, "skills")) {
		t.Fatalf("expected temp staged skill root, got %q", adapter.stagedSkillRoot)
	}
	if _, err := os.Stat(filepath.Join(adapter.stagedSkillRoot, "01-code_builder", "SKILL.md")); err != nil {
		t.Fatalf("expected staged code_builder skill: %v", err)
	}
}

func TestExecuteRunOnceSkipsWorkspaceForOrdinaryAgent(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "ticket", ID: "T-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "summarize"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{}
	adapter := &recordingRuntimeAdapter{}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Workspaces:           workspaces,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if provider.prepareCalls != 0 {
		t.Fatalf("ordinary run prepared workspace %d times", provider.prepareCalls)
	}
	if adapter.lease != nil {
		t.Fatalf("ordinary run received workspace lease: %#v", adapter.lease)
	}
}

func TestExecuteRunOnceUsesHostPreparedWorkspace(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.AllowedTargets = []string{"repository"}
	agent.ExecutionConfig = json.RawMessage(`{"workspace":{"mode":"host_prepared"}}`)
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	run := &agentcore.AgentRun{
		AppID:         "app-a",
		AgentID:       agent.ID,
		Target:        agentcore.TargetRef{Type: "repository", ID: "repo-1"},
		RuntimeKind:   agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeLightweight,
		Input:         agentcore.RunInput{Instructions: "change code"},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	provider := &recordingWorkspaceProvider{lease: agentcore.WorkspaceLease{
		ID:            "lease-1",
		Provider:      "test",
		RootPath:      "/tmp/repo",
		CleanupPolicy: workspace.CleanupOnTerminal,
	}}
	adapter := &recordingRuntimeAdapter{}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", provider); err != nil {
		t.Fatalf("register workspace: %v", err)
	}
	eng := New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(adapter),
		Tools:                tools.NewRegistry(),
		Targets:              host.NewStaticContextProvider(),
		Workspaces:           workspaces,
	})

	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("execute run: %v", err)
	}
	if provider.prepareCalls != 1 || provider.finalizeCalls != 1 || provider.cleanupCalls != 1 {
		t.Fatalf("unexpected workspace lifecycle: prepare=%d finalize=%d cleanup=%d", provider.prepareCalls, provider.finalizeCalls, provider.cleanupCalls)
	}
	if adapter.lease == nil || adapter.lease.ID != "lease-1" {
		t.Fatalf("adapter did not receive workspace lease: %#v", adapter.lease)
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.WorkspaceLease == nil || stored.WorkspaceLease.ID != "lease-1" {
		t.Fatalf("workspace lease was not persisted: %#v", stored.WorkspaceLease)
	}
	if provider.finalizeOutcome != agentcore.RunStatusCompleted {
		t.Fatalf("finalize outcome = %q", provider.finalizeOutcome)
	}
}

type recordingRuntimeAdapter struct {
	kind              string
	lease             *agentcore.WorkspaceLease
	calls             int
	skillRefs         []agentcore.SkillRef
	skillInstructions string
	skillPolicy       skills.Policy
	stagedSkillRoot   string
}

func (a *recordingRuntimeAdapter) Kind() string {
	if a.kind != "" {
		return a.kind
	}
	return agentcore.RuntimeNativeSDK
}

func (a *recordingRuntimeAdapter) Execute(execCtx *runtime.ExecutionContext) (*runtime.Result, error) {
	a.calls++
	a.lease = execCtx.WorkspaceLease
	a.skillRefs = append([]agentcore.SkillRef(nil), execCtx.SkillRefs...)
	a.skillInstructions = execCtx.SkillInstructions
	a.skillPolicy = execCtx.SkillPolicy
	a.stagedSkillRoot = execCtx.StagedSkillRoot
	return &runtime.Result{
		AssistantMessage: "done",
		OutputSummary:    json.RawMessage(`{"ok":true}`),
	}, nil
}

func skillKeys(refs []agentcore.SkillRef) []string {
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, ref.Key)
	}
	return keys
}

type pausingRuntimeAdapter struct {
	calls int
}

func (a *pausingRuntimeAdapter) Kind() string {
	return agentcore.RuntimeNativeSDK
}

func (a *pausingRuntimeAdapter) Execute(execCtx *runtime.ExecutionContext) (*runtime.Result, error) {
	a.calls++
	if a.calls == 1 {
		_ = execCtx.InteractionBroker.RequestInteraction(execCtx.Context, agentcore.AgentRunInteraction{
			InteractionKind: "request_user_input",
			Status:          "pending",
			Title:           "Input requested",
			Summary:         "Who owns this?",
			RequestPayload:  json.RawMessage(`{"request_schema":"request_user_input_v1"}`),
		})
		return &runtime.Result{
			AssistantMessage: "Need input.",
			OutputSummary:    json.RawMessage(`{"native_messages":[{"role":"user","content":"ask"}]}`),
			AwaitingInput:    true,
		}, nil
	}
	return &runtime.Result{
		AssistantMessage: "Done.",
		OutputSummary:    json.RawMessage(`{"ok":true}`),
	}, nil
}

type recordingWorkspaceProvider struct {
	lease           agentcore.WorkspaceLease
	prepareCalls    int
	finalizeCalls   int
	cleanupCalls    int
	finalizeOutcome string
}

func (p *recordingWorkspaceProvider) PrepareWorkspace(_ context.Context, _ workspace.PrepareRequest) (*agentcore.WorkspaceLease, error) {
	p.prepareCalls++
	lease := p.lease
	if lease.ID == "" {
		lease = agentcore.WorkspaceLease{ID: "lease-1", RootPath: "/tmp/repo"}
	}
	return &lease, nil
}

func (p *recordingWorkspaceProvider) FinalizeWorkspace(_ context.Context, req workspace.FinalizeRequest) (*workspace.FinalizeResult, error) {
	p.finalizeCalls++
	p.finalizeOutcome = req.Outcome
	return &workspace.FinalizeResult{}, nil
}

func (p *recordingWorkspaceProvider) CleanupWorkspace(_ context.Context, _ workspace.CleanupRequest) error {
	p.cleanupCalls++
	return nil
}

func testAgent(appID string) agentcore.Agent {
	return agentcore.Agent{
		AppID:                 appID,
		Name:                  "Test Agent",
		RuntimeKind:           agentcore.RuntimeNativeSDK,
		AllowedTools:          []string{"get_context"},
		AllowedTargets:        []string{"ticket"},
		ApprovalMode:          agentcore.ApprovalModeNever,
		DefaultInvocationMode: agentcore.InvocationAutonomous,
	}
}

func testEngine(mem *store.Memory, targets host.TargetContextProvider) *Engine {
	return New(Config{
		DefaultExecutionMode: ExecutionModeLightweight,
		Store:                mem,
		Runtimes:             runtime.NewRegistry(runtime.NewNativeAdapter(), runtime.NewCodexAdapter()),
		Tools:                tools.NewRegistry(),
		Targets:              targets,
	})
}

func waitForRunStatus(t *testing.T, mem *store.Memory, appID, runID, status string) *agentcore.AgentRun {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := mem.GetRun(context.Background(), appID, runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run != nil && run.Status == status {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, _ := mem.GetRun(context.Background(), appID, runID)
	t.Fatalf("run did not reach status %q; got %#v", status, run)
	return nil
}
