package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
	"go.temporal.io/api/serviceerror"
)

// unreadableCheckpointStore keeps every Memory behaviour but fails to load the
// native checkpoint, the way a corrupt or truncated payload does in production.
type unreadableCheckpointStore struct{ *store.Memory }

func (unreadableCheckpointStore) LoadNativeState(context.Context, string, string) (*agentcore.NativeState, error) {
	return nil, errors.New("checkpoint unreadable")
}

func TestCancelRunTearsDownWhenCheckpointUnreadable(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID: "run-broken-checkpoint", AppID: "app-a", AgentID: "agent-1",
		Target: agentcore.TargetRef{Type: "workspace", ID: "ws-1"}, ExecutionMode: ExecutionModeLightweight, Status: agentcore.RunStatusRunning,
	}
	if err := mem.CreateRunWithMCP(ctx, run, []agentcore.RunMCPServer{{
		ServerID: "server-1", ServerName: "github", Transport: agentcore.MCPTransportStreamableHTTP,
		URL: "https://mcp.example.com/mcp", Tools: []agentcore.RunMCPTool{{Name: "get_issue", Access: agentcore.MCPToolAccessRead}},
		EncryptedCredential: []byte{1, 2, 3},
	}}); err != nil {
		t.Fatal(err)
	}
	events := &recordingEngineEventSink{}
	eng := New(Config{Store: unreadableCheckpointStore{mem}, EventSink: events})

	cancelled, err := eng.CancelRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("cancel run with unreadable checkpoint: %v", err)
	}
	if cancelled.Status != agentcore.RunStatusCancelled {
		t.Fatalf("expected cancelled run, got %#v", cancelled)
	}
	servers, err := mem.ListRunMCPServers(ctx, "app-a", run.ID)
	if err != nil || len(servers) != 1 || len(servers[0].EncryptedCredential) != 0 {
		t.Fatalf("credential was not cleared: %#v err=%v", servers, err)
	}
	var sawCancelled bool
	for _, event := range events.snapshot() {
		if event.Type == "run.cancelled" {
			sawCancelled = true
		}
	}
	if !sawCancelled {
		t.Fatalf("run.cancelled was not emitted: %#v", events.snapshot())
	}

	// A retry against the already cancelled run must not surface the checkpoint error either.
	if _, err := eng.CancelRun(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("cancel retry: %v", err)
	}
}

type closedWorkflowDurable struct {
	recordingDurableExecutor
	cancelErr   error
	cancelCalls int
}

func (d *closedWorkflowDurable) CancelRun(context.Context, *agentcore.AgentRun) error {
	d.cancelCalls++
	return d.cancelErr
}

func TestResumeRunIdleTimeoutCompletesWhenWorkflowAlreadyClosed(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{
		ID: "run-idle-durable", AppID: "app-a", AgentID: agent.ID,
		Target: agentcore.TargetRef{Type: "conversation", ID: "C-1"}, RuntimeKind: agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeDurable, Status: agentcore.RunStatusPaused, PauseReason: agentcore.PauseReasonUserMessage,
		Input: agentcore.RunInput{TurnPolicy: agentcore.TurnPolicy{Mode: agentcore.TurnPolicyPauseAfterAssist, IdleTimeoutSeconds: 1}},
	}
	if err := mem.CreateRunWithMCP(ctx, run, []agentcore.RunMCPServer{{
		ServerID: "server-1", ServerName: "github", Transport: agentcore.MCPTransportStreamableHTTP,
		URL: "https://mcp.example.com/mcp", Tools: []agentcore.RunMCPTool{{Name: "get_issue", Access: agentcore.MCPToolAccessRead}},
		EncryptedCredential: []byte{1, 2, 3},
	}}); err != nil {
		t.Fatal(err)
	}
	durable := &closedWorkflowDurable{cancelErr: serviceerror.NewNotFound("workflow execution already completed")}
	events := &recordingEngineEventSink{}
	eng := New(Config{Store: mem, Durable: durable, EventSink: events, DefaultExecutionMode: ExecutionModeDurable})
	time.Sleep(1100 * time.Millisecond)

	_, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{Intent: "reply", Content: "hello", MessageProvenance: "human", ExternalActorID: "user-1"})
	if err == nil || !strings.Contains(err.Error(), "run idle timeout expired") {
		t.Fatalf("expected idle timeout error, got %v", err)
	}
	if durable.cancelCalls != 1 {
		t.Fatalf("durable cancel calls = %d", durable.cancelCalls)
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != agentcore.RunStatusCompleted || stored.CompletedAt == nil || stored.PauseReason != agentcore.PauseReasonNone {
		t.Fatalf("expected completed run, got %#v", stored)
	}
	servers, err := mem.ListRunMCPServers(ctx, "app-a", run.ID)
	if err != nil || len(servers) != 1 || len(servers[0].EncryptedCredential) != 0 {
		t.Fatalf("credential was not cleared: %#v err=%v", servers, err)
	}
	var completed bool
	for _, event := range events.snapshot() {
		if event.Type == "run.completed" && event.Data["reason"] == "idle_timeout" {
			completed = true
		}
	}
	if !completed {
		t.Fatalf("run.completed(idle_timeout) was not emitted: %#v", events.snapshot())
	}
}

func TestResumeRunIdleTimeoutLeavesRunPausedWhenDurableCancelFails(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{
		ID: "run-idle-durable-fail", AppID: "app-a", AgentID: agent.ID,
		Target: agentcore.TargetRef{Type: "conversation", ID: "C-1"}, RuntimeKind: agentcore.RuntimeNativeSDK,
		ExecutionMode: ExecutionModeDurable, Status: agentcore.RunStatusPaused, PauseReason: agentcore.PauseReasonUserMessage,
		Input: agentcore.RunInput{TurnPolicy: agentcore.TurnPolicy{Mode: agentcore.TurnPolicyPauseAfterAssist, IdleTimeoutSeconds: 1}},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	durable := &closedWorkflowDurable{cancelErr: errors.New("temporal unavailable")}
	eng := New(Config{Store: mem, Durable: durable, DefaultExecutionMode: ExecutionModeDurable})
	time.Sleep(1100 * time.Millisecond)

	_, err := eng.ResumeRun(ctx, "app-a", run.ID, ResumePayload{Intent: "reply", Content: "hello"})
	if err == nil || !strings.Contains(err.Error(), "temporal unavailable") {
		t.Fatalf("expected durable cancel error, got %v", err)
	}
	stored, err := mem.GetRun(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != agentcore.RunStatusPaused || stored.CompletedAt != nil {
		t.Fatalf("run must stay paused and retryable when the workflow could not be stopped: %#v", stored)
	}
}

func TestWorkspaceManagerPersistsBranchWithoutRepositorySpec(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	run := &agentcore.AgentRun{
		ID: "run-local-cli", AppID: "app-a", AgentID: agent.ID, Status: agentcore.RunStatusRunning,
		Input: agentcore.RunInput{Metadata: map[string]interface{}{}},
		WorkspaceLease: &agentcore.WorkspaceLease{
			ID: "lease", Provider: "local-cli", RootPath: t.TempDir(),
			Metadata: map[string]interface{}{"work_branch": "main"},
		},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	manager := engineWorkspaceManager{engine: New(Config{Store: mem}), agent: &agent, run: run}

	if err := manager.SetRepositoryBranch(ctx, "feature/local"); err != nil {
		t.Fatalf("set branch without spec: %v", err)
	}
	stored, err := mem.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stringFromMap(stored.WorkspaceLease.Metadata, "work_branch"); got != "feature/local" {
		t.Fatalf("lease work branch = %q", got)
	}
	if _, ok := stored.WorkspaceLease.Metadata["repository_spec"]; ok {
		t.Fatalf("spec-less lease grew a repository spec: %#v", stored.WorkspaceLease.Metadata)
	}
	if stored.Input.Metadata["work_branch"] != "feature/local" {
		t.Fatalf("run metadata branch = %#v", stored.Input.Metadata["work_branch"])
	}

	if err := manager.SetRepositoryDetachedHead(ctx, "abc123"); err != nil {
		t.Fatalf("set detached head without spec: %v", err)
	}
	stored, err = mem.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stringFromMap(stored.WorkspaceLease.Metadata, "detached_head"); got != "abc123" {
		t.Fatalf("lease detached head = %q", got)
	}
	if _, ok := stored.WorkspaceLease.Metadata["work_branch"]; ok {
		t.Fatalf("work_branch survived detached checkout: %#v", stored.WorkspaceLease.Metadata)
	}
	if stored.Input.Metadata["detached_head"] != "abc123" || stored.Input.Metadata["work_branch"] != nil {
		t.Fatalf("run metadata was not switched to detached head: %#v", stored.Input.Metadata)
	}
}

func TestWorkspaceManagerBranchUpdateDoesNotReviveTerminalRun(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	run := &agentcore.AgentRun{
		ID: "run-cancelled-branch", AppID: "app-a", AgentID: agent.ID, Status: agentcore.RunStatusRunning,
		Input:          agentcore.RunInput{Metadata: map[string]interface{}{}},
		WorkspaceLease: &agentcore.WorkspaceLease{ID: "lease", Provider: "local-cli", RootPath: t.TempDir(), Metadata: map[string]interface{}{}},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	// The worker keeps its own copy while an operator cancels the run.
	workerRun := *run
	if _, err := New(Config{Store: mem}).CancelRun(ctx, "app-a", run.ID); err != nil {
		t.Fatal(err)
	}
	manager := engineWorkspaceManager{engine: New(Config{Store: mem}), agent: &agent, run: &workerRun}

	err := manager.SetRepositoryBranch(ctx, "feature/late")
	if err == nil || !strings.Contains(err.Error(), "run is no longer active") {
		t.Fatalf("expected inactive run error, got %v", err)
	}
	if err := manager.SetRepositoryDetachedHead(ctx, "abc123"); err == nil || !strings.Contains(err.Error(), "run is no longer active") {
		t.Fatalf("expected inactive run error, got %v", err)
	}
	stored, err := mem.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != agentcore.RunStatusCancelled || stored.CompletedAt == nil {
		t.Fatalf("branch update overwrote the cancelled run: %#v", stored)
	}
	if stored.Input.Metadata["work_branch"] != nil {
		t.Fatalf("branch was persisted onto a cancelled run: %#v", stored.Input.Metadata)
	}
}

func TestWorkspaceManagerBranchUpdateWritesOntoStoredRun(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	run := &agentcore.AgentRun{
		ID: "run-stale-branch", AppID: "app-a", AgentID: agent.ID, Status: agentcore.RunStatusRunning,
		Input:          agentcore.RunInput{Metadata: map[string]interface{}{}},
		WorkspaceLease: &agentcore.WorkspaceLease{ID: "lease", Provider: "local-cli", RootPath: t.TempDir(), Metadata: map[string]interface{}{}},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	workerRun := *run
	// Another actor moved the run to paused with an error message the worker has not seen.
	paused := *run
	paused.Status = agentcore.RunStatusPaused
	paused.PauseReason = agentcore.PauseReasonUserMessage
	paused.ErrorMessage = "stored-only"
	if err := mem.UpdateRun(ctx, &paused); err != nil {
		t.Fatal(err)
	}
	manager := engineWorkspaceManager{engine: New(Config{Store: mem}), agent: &agent, run: &workerRun}
	if err := manager.SetRepositoryBranch(ctx, "feature/fresh"); err != nil {
		t.Fatal(err)
	}
	stored, err := mem.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != agentcore.RunStatusPaused || stored.ErrorMessage != "stored-only" {
		t.Fatalf("stale worker copy overwrote stored lifecycle state: %#v", stored)
	}
	if stored.Input.Metadata["work_branch"] != "feature/fresh" || stringFromMap(stored.WorkspaceLease.Metadata, "work_branch") != "feature/fresh" {
		t.Fatalf("branch was not persisted: %#v %#v", stored.Input.Metadata, stored.WorkspaceLease.Metadata)
	}
}

func TestPushRepositoryReportsUnsupportedWithoutHostProvider(t *testing.T) {
	ctx := context.Background()
	agent := testAgent("app-a")
	lease := &agentcore.WorkspaceLease{ID: "lease", Provider: "repository", RootPath: t.TempDir()}
	run := &agentcore.AgentRun{ID: "run-push", AppID: "app-a", AgentID: agent.ID, WorkspaceLease: lease}

	// No workspace registry configured (embedded / local CLI engine).
	manager := engineWorkspaceManager{engine: New(Config{Store: store.NewMemory()}), agent: &agent, run: run}
	if _, err := manager.PushRepository(ctx, "msg"); !errors.Is(err, workspace.ErrDirectPublicationUnsupported) {
		t.Fatalf("nil registry: expected ErrDirectPublicationUnsupported, got %v", err)
	}

	// Registry present but the lease was not prepared by the repository provider.
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register("app-a", &recordingWorkspaceProvider{}); err != nil {
		t.Fatal(err)
	}
	hostRun := *run
	hostRun.WorkspaceLease = &agentcore.WorkspaceLease{ID: "lease", Provider: "host-repository", RootPath: lease.RootPath}
	manager = engineWorkspaceManager{engine: New(Config{Store: store.NewMemory(), Workspaces: workspaces}), agent: &agent, run: &hostRun}
	if _, err := manager.PushRepository(ctx, "msg"); !errors.Is(err, workspace.ErrDirectPublicationUnsupported) {
		t.Fatalf("non-repository lease: expected ErrDirectPublicationUnsupported, got %v", err)
	}

	// Registry present, repository lease, but no agent attached to the manager.
	manager = engineWorkspaceManager{engine: New(Config{Store: store.NewMemory(), Workspaces: workspaces}), run: run}
	if _, err := manager.PushRepository(ctx, "msg"); !errors.Is(err, workspace.ErrDirectPublicationUnsupported) {
		t.Fatalf("nil agent: expected ErrDirectPublicationUnsupported, got %v", err)
	}

	// Missing checkout is still a hard error, not a fallback.
	manager = engineWorkspaceManager{engine: New(Config{Store: store.NewMemory()}), agent: &agent, run: &agentcore.AgentRun{ID: "no-lease", AppID: "app-a"}}
	if _, err := manager.PushRepository(ctx, "msg"); err == nil || errors.Is(err, workspace.ErrDirectPublicationUnsupported) {
		t.Fatalf("missing lease: expected checkout error, got %v", err)
	}
}

func TestReconcileDurableRunsIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pauseReason string
		timeout     int
		cancelErr   error
		wantRetired bool
		wantCancel  bool
	}{
		{name: "expired open workflow", pauseReason: agentcore.PauseReasonUserMessage, timeout: 1, wantRetired: true, wantCancel: true},
		{name: "already closed workflow", pauseReason: agentcore.PauseReasonUserMessage, timeout: 1, cancelErr: serviceerror.NewNotFound("closed"), wantRetired: true, wantCancel: true},
		{name: "cancel failure remains retryable", pauseReason: agentcore.PauseReasonUserMessage, timeout: 1, cancelErr: errors.New("temporal unavailable"), wantCancel: true},
		{name: "unexpired chat", pauseReason: agentcore.PauseReasonUserMessage, timeout: 3600},
		{name: "no timeout", pauseReason: agentcore.PauseReasonUserMessage},
		{name: "approval", pauseReason: agentcore.PauseReasonHumanApproval, timeout: 1},
		{name: "human input", pauseReason: agentcore.PauseReasonHumanInput, timeout: 1},
		{name: "manual pause", pauseReason: agentcore.PauseReasonManual, timeout: 1},
		{name: "auth", pauseReason: agentcore.PauseReasonAuth, timeout: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			mem := store.NewMemory()
			run := &agentcore.AgentRun{
				ID: "idle", AppID: "app-a", AgentID: "agent-1",
				ExecutionMode: ExecutionModeDurable, Status: agentcore.RunStatusPaused, PauseReason: tc.pauseReason,
				Input: agentcore.RunInput{TurnPolicy: agentcore.TurnPolicy{Mode: agentcore.TurnPolicyPauseAfterAssist, IdleTimeoutSeconds: tc.timeout}},
			}
			if err := mem.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			durable := &closedWorkflowDurable{cancelErr: tc.cancelErr}
			events := &recordingEngineEventSink{}
			eng := New(Config{Store: mem, Durable: durable, EventSink: events})
			time.Sleep(1100 * time.Millisecond)
			count, err := eng.ReconcileDurableRuns(ctx, time.Now())
			wantError := tc.cancelErr != nil && !tc.wantRetired
			if (err != nil) != wantError {
				t.Fatalf("unexpected reconciliation error: %v", err)
			}
			if (durable.cancelCalls == 1) != tc.wantCancel {
				t.Fatalf("cancel calls = %d", durable.cancelCalls)
			}
			stored, err := mem.GetRun(ctx, run.AppID, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRetired {
				if count != 1 || stored.Status != agentcore.RunStatusCompleted || stored.CompletedAt == nil {
					t.Fatalf("expected retired chat: count=%d run=%#v", count, stored)
				}
				var completed bool
				for _, event := range events.snapshot() {
					if event.Type == "run.completed" && event.Data["reason"] == "idle_timeout" {
						completed = true
					}
				}
				if !completed {
					t.Fatal("missing idle_timeout completion event")
				}
				if count, err := eng.ReconcileDurableRuns(ctx, time.Now()); count != 0 || err != nil {
					t.Fatalf("repeat reconciliation: count=%d err=%v", count, err)
				}
			} else if count != 0 || stored.Status != agentcore.RunStatusPaused || stored.CompletedAt != nil {
				t.Fatalf("expected unchanged pause: count=%d run=%#v", count, stored)
			}
		})
	}
}

func TestReconcileIdleChatRechecksStalePage(t *testing.T) {
	ctx := context.Background()
	mem := store.NewMemory()
	run := &agentcore.AgentRun{
		ID: "resumed", AppID: "app-a", ExecutionMode: ExecutionModeDurable,
		Status: agentcore.RunStatusRunning,
		Input:  agentcore.RunInput{TurnPolicy: agentcore.TurnPolicy{Mode: agentcore.TurnPolicyPauseAfterAssist, IdleTimeoutSeconds: 1}},
	}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	stale := *run
	stale.Status, stale.PauseReason = agentcore.RunStatusPaused, agentcore.PauseReasonUserMessage
	stale.UpdatedAt = time.Now().Add(-time.Hour)
	durable := &closedWorkflowDurable{}
	eng := New(Config{Store: mem, Durable: durable})
	count, err := eng.reconcileDurableRun(ctx, &stale, time.Now())
	if err != nil || count != 0 || durable.cancelCalls != 0 {
		t.Fatalf("resumed run was retired: count=%d cancels=%d err=%v", count, durable.cancelCalls, err)
	}
}
