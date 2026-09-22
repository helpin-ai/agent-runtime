package durable

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/temporalclient"
	"github.com/helpin-ai/agent-runtime/internal/tools"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

type stagingRepository string

func (s stagingRepository) ResolveRepositoryWorkspace(context.Context, workspace.PrepareRequest) (*workspace.RepositoryWorkspaceSpec, error) {
	return &workspace.RepositoryWorkspaceSpec{CloneURL: string(s), BaseBranch: "main"}, nil
}

// The live harness exercises the real workflow, activities, engine, database
// locks and Git provider; this deterministic adapter replaces only paid LLM I/O.
type stagingRecoveryAdapter struct{ crash bool }

func (stagingRecoveryAdapter) Kind() string { return agentcore.RuntimeNativeSDK }
func (a stagingRecoveryAdapter) Execute(x *runtime.ExecutionContext) (*runtime.Result, error) {
	file := filepath.Join(x.WorkspaceLease.RootPath, "uncommitted.txt")
	if a.crash {
		if err := os.WriteFile(file, []byte("lost edit"), 0600); err != nil {
			return nil, err
		}
		fmt.Println("SMOKE_READY_TO_KILL", x.Run.ID, workspace.Session(x.Context))
		<-x.Context.Done()
		return nil, x.Context.Err()
	}
	if x.Run.Input.Metadata[workspace.RecoveryMetadataKey] == nil {
		return nil, fmt.Errorf("replacement has no recovery marker")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		return nil, fmt.Errorf("replacement reused stale local edit")
	}
	if _, err := os.Stat(filepath.Join(x.WorkspaceLease.RootPath, ".git")); err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, []byte("rebuilt edit"), 0600); err != nil {
		return nil, err
	}
	fmt.Println("SMOKE_RECOVERED_FRESH_CHECKOUT", x.Run.ID, workspace.Session(x.Context))
	return &runtime.Result{AssistantMessage: "ephemeral recovery verified", TurnFinished: true}, nil
}

func TestEphemeralWorkerStaging(t *testing.T) {
	role, name := os.Getenv("AGENT_RUNTIME_EPHEMERAL_SMOKE_ROLE"), os.Getenv("AGENT_RUNTIME_EPHEMERAL_SMOKE_ID")
	if role == "" || name == "" {
		t.Skip("explicit isolated staging smoke role/id required")
	}
	if role != "crash" && role != "recover" && role != "controller" {
		t.Fatal("invalid smoke role")
	}
	t.Setenv(workspace.StorageModeEnv, "ephemeral")
	s, err := store.OpenSQL(store.SQLConfig{Driver: "postgres", DSN: os.Getenv("DATABASE_URL")})
	if err != nil {
		t.Fatal("staging database connection failed")
	}
	db, _ := s.DB().DB()
	defer db.Close()
	c, err := client.Dial(temporalclient.BuildOptionsFromEnv(os.Getenv("TEMPORAL_ADDRESS")))
	if err != nil {
		t.Fatal("staging Temporal connection failed")
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if role == "controller" {
		agent := &agentcore.Agent{ID: name, AppID: name, Name: "Disposable ephemeral recovery smoke", RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: []string{"run_command"}, AllowedTargets: []string{"repository"}, ApprovalMode: agentcore.ApprovalModeNever, ExecutionConfig: []byte(`{"workspace":{"mode":"repository"}}`)}
		if err := s.CreateAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
		run := &agentcore.AgentRun{ID: name, AppID: name, AgentID: name, RuntimeKind: agentcore.RuntimeNativeSDK, Status: agentcore.RunStatusQueued, InvocationMode: agentcore.InvocationAutonomous, Target: agentcore.TargetRef{Type: "repository", ID: "fixture"}, Input: agentcore.RunInput{Instructions: "Verify fresh checkout recovery", Metadata: map[string]any{engine.CodingMetadataKey: true}}}
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		started, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: name, TaskQueue: name}, AgentRunWorkflow, AgentRunWorkflowInput{AppID: name, RunID: name, EphemeralWorkspace: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Log("isolated workflow started", name)
		defer c.CancelWorkflow(context.Background(), name, started.GetRunID())
		if err := started.Get(ctx, nil); err != nil {
			t.Fatal(err)
		}
		run, err = s.GetRun(ctx, name, name)
		if err != nil || run.Status != agentcore.RunStatusCompleted || run.Input.Metadata[workspace.RecoveryMetadataKey] == nil {
			t.Fatalf("recovered run did not complete: %v %+v", err, run)
		}
		t.Log("replacement worker completed the run with a persisted recovery marker")
		return
	}
	remote := t.TempDir()
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"-c", "user.name=Smoke", "-c", "user.email=smoke@example.invalid", "commit", "--allow-empty", "-m", "fixture"}} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = remote
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create fixture: %v %s", err, out)
		}
	}
	registry := workspace.NewRegistry()
	if err := registry.Register(name, workspace.RepositoryProvider{RootDir: workspace.WorkspaceRoot(), SpecProvider: stagingRepository(remote)}); err != nil {
		t.Fatal(err)
	}
	targets := host.NewStaticContextProvider()
	targets.Register(name, agentcore.TargetRef{Type: "repository", ID: "fixture"}, host.TargetContext{Summary: "Disposable smoke fixture"})
	e := engine.New(engine.Config{Store: s, CodingWorker: true, Workspaces: registry, Targets: targets, Tools: tools.NewRegistry(), Runtimes: runtime.NewRegistry(stagingRecoveryAdapter{crash: role == "crash"})})
	w := worker.New(c, name, worker.Options{EnableSessionWorker: true, MaxConcurrentActivityExecutionSize: 4, MaxConcurrentSessionExecutionSize: 16, WorkerStopTimeout: time.Second})
	RegisterAgentRunWorker(w, NewAgentRunActivities(s, e))
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	t.Log("isolated worker ready", role, name)
	<-ctx.Done()
}
