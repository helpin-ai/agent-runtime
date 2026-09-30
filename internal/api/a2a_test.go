package api

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// a2aTargets returns fresh connection details on every resolve, like the
// host's context endpoint does.
type a2aTargets struct{ cardURL string }

func (p a2aTargets) ResolveTarget(_ context.Context, _ string, target agentcore.TargetRef) (*host.TargetContext, error) {
	return &host.TargetContext{Target: target, Summary: "Task " + target.ID, Data: map[string]interface{}{
		"a2a": map[string]interface{}{
			"external_agent_id":     "ext-1",
			"name":                  "Hermes",
			"card_url":              p.cardURL,
			"auth":                  map[string]interface{}{"type": "bearer", "token": "secret"},
			"allow_private_network": true,
		},
	}}, nil
}

func TestAPIExternalA2AAgentPausesAndCompletes(t *testing.T) {
	executor := a2asrv.AgentExecutorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if execCtx.StoredTask == nil {
				if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
					return
				}
				question := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("Which environment?"))
				yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, question), nil)
				return
			}
			reply := execCtx.Message.Parts[0].Text()
			answer := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, a2a.NewTextPart("Used "+reply+"."))
			yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, answer), nil)
		}
	})
	mux := http.NewServeMux()
	remote := httptest.NewServer(mux)
	defer remote.Close()
	mux.Handle("/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(&a2a.AgentCard{
		Name:                "Hermes",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(remote.URL+"/rpc", a2a.TransportProtocolJSONRPC)},
	}))
	mux.Handle("/rpc", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor)))

	mem := store.NewMemory()
	eng := engine.New(engine.Config{
		DefaultExecutionMode:       engine.ExecutionModeLightweight,
		ManualLightweightExecution: true,
		Store:                      mem,
		Runtimes:                   runtime.NewRegistry(runtime.NewNativeAdapter(), runtime.NewA2AAdapter()),
		Tools:                      tools.NewRegistry(),
		Targets:                    a2aTargets{cardURL: remote.URL + "/.well-known/agent-card.json"},
		// External agents carry no model, so this app-wide requirement must
		// not block them.
		RequireRunModelCredentials: func(string) bool { return true },
	})
	handler := NewServer(Config{Engine: eng, Store: mem, AllowAnonymous: true})

	postJSON[map[string]any](t, handler, "/v1/agents", map[string]any{"app_id": "app-a", "name": "Old", "runtime_kind": "codex"}, http.StatusBadRequest)
	agent := postJSON[agentcore.Agent](t, handler, "/v1/agents", map[string]any{
		"app_id": "app-a", "name": "Hermes", "runtime_kind": "a2a", "allowed_targets": []string{"task"},
		"execution_config": map[string]any{"external_a2a_agent_id": "ext-1"},
	}, http.StatusCreated)
	if agent.RuntimeKind != agentcore.RuntimeA2A {
		t.Fatalf("runtime kind was not kept: %q", agent.RuntimeKind)
	}
	run := postJSON[agentcore.AgentRun](t, handler, "/v1/runs", map[string]any{
		"app_id": "app-a", "agent_id": agent.ID, "target": map[string]string{"type": "task", "id": "T-1"}, "instructions": "Record the demo.",
	}, http.StatusAccepted)

	ctx := context.Background()
	first, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if first == nil || !first.AwaitingInput {
		t.Fatalf("first turn did not wait for input: %+v", first)
	}
	paused, _ := mem.GetRun(ctx, "app-a", run.ID)
	if paused.Status != agentcore.RunStatusPaused || paused.PauseReason != agentcore.PauseReasonHumanInput {
		t.Fatalf("run was not paused for input: %s/%s", paused.Status, paused.PauseReason)
	}

	postJSON[agentcore.AgentRun](t, handler, "/v1/runs/"+run.ID+"/resume?app_id=app-a", map[string]any{
		"intent": "reply", "content": "staging", "resume_id": "resume-1",
	}, http.StatusOK)
	if _, err := eng.ExecuteRunOnce(ctx, "app-a", run.ID); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	completed, _ := mem.GetRun(ctx, "app-a", run.ID)
	if completed.Status != agentcore.RunStatusCompleted {
		t.Fatalf("run did not complete: %s (%s)", completed.Status, completed.ErrorMessage)
	}
	messages, err := mem.ListMessages(ctx, "app-a", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var assistant []string
	for _, message := range messages {
		if message.Role == "assistant" {
			assistant = append(assistant, message.Content)
		}
	}
	if len(assistant) != 2 || assistant[0] != "Which environment?" || assistant[1] != "Used staging." {
		t.Fatalf("unexpected assistant transcript: %v", assistant)
	}
}
