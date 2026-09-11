package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/host"
	"github.com/helpin-ai/agent-runtime/internal/runtime"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

const engineAnswer = "Mermaid, nwdiag, and Excalidraw belong in the editor diagrams guide."

type answerModel struct{}

func (answerModel) ResolveNativeModel(context.Context, *runtime.ExecutionContext, []tools.Definition) (runtime.NativeModel, error) {
	return answerModel{}, nil
}
func (answerModel) Generate(context.Context, runtime.NativeModelRequest) (*runtime.NativeModelResponse, error) {
	return &runtime.NativeModelResponse{Message: runtime.NativeMessage{Role: "assistant", Content: "Here is the complete picture:", Blocks: []runtime.NativeBlock{
		{Type: "text", Text: "Here is the complete picture:"},
		{Type: "tool_call", ToolCallID: "finish-1", ToolName: "finish_turn", Input: json.RawMessage(`{"outcome":"completed","summary":"Mermaid, nwdiag, and Excalidraw belong in the editor diagrams guide."}`)},
	}}}, nil
}

type answerFailureStore struct{ agentcore.Store }

func (s answerFailureStore) AppendMessage(ctx context.Context, m *agentcore.AgentRunMessage) error {
	if m.Content == engineAnswer {
		return errors.New("storage unavailable")
	}
	return s.Store.AppendMessage(ctx, m)
}

func TestExecuteRunOncePersistsAnswerBeforeCompletionPause(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivered", true: "storage failure"}[fail], func(t *testing.T) {
			ctx := context.Background()
			mem := store.NewMemory()
			agent := testAgent("app-a")
			agent.ExecutionConfig = nil
			if err := mem.CreateAgent(ctx, &agent); err != nil {
				t.Fatal(err)
			}
			run := &agentcore.AgentRun{ID: "answer-run", AppID: agent.AppID, AgentID: agent.ID, Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"}, RuntimeKind: agentcore.RuntimeNativeSDK, ExecutionMode: ExecutionModeLightweight, Status: agentcore.RunStatusQueued,
				Input: agentcore.RunInput{TurnPolicy: agentcore.TurnPolicy{Mode: agentcore.TurnPolicyPauseAfterAssist, CompletionMode: agentcore.TurnCompletionExplicit}},
			}
			if err := mem.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			var runtimeStore agentcore.Store = mem
			if fail {
				runtimeStore = answerFailureStore{mem}
			}
			events := &recordingEngineEventSink{}
			eng := New(Config{Store: runtimeStore, EventSink: events, Targets: host.NewStaticContextProvider(), Tools: tools.NewRegistry(), Runtimes: runtime.NewRegistry(runtime.NewNativeAdapterWithConfig(runtime.NativeConfig{ModelFactory: answerModel{}}))})
			result, err := eng.ExecuteRunOnce(ctx, run.AppID, run.ID)
			saved, getErr := mem.GetRun(ctx, run.AppID, run.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			timeline := events.snapshot()
			if fail {
				if err == nil || saved.Status != agentcore.RunStatusFailed {
					t.Fatalf("storage failure incorrectly settled turn: %+v %v", saved, err)
				}
				for _, event := range timeline {
					if event.Type == "run.paused" || event.Type == "run.completed" {
						t.Fatalf("published completion after storage failure: %+v", event)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if saved.Status != agentcore.RunStatusPaused || saved.PauseReason != agentcore.PauseReasonUserMessage {
				t.Fatalf("expected chat pause: %+v", saved)
			}
			answerIndex, pauseIndex := -1, -1
			for i, event := range timeline {
				if event.Type == "assistant_message_completed" && event.Data["content"] == engineAnswer {
					answerIndex = i
				}
				if event.Type == "run.paused" {
					pauseIndex = i
				}
			}
			if answerIndex < 0 || pauseIndex <= answerIndex {
				t.Fatalf("answer must precede pause, answer=%d pause=%d", answerIndex, pauseIndex)
			}
			messages, err := mem.ListMessages(ctx, run.AppID, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var count int
			for _, m := range messages {
				if m.RuntimeMessageID == result.AssistantMessageID && m.Content == engineAnswer {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("expected one durable answer, got %d", count)
			}
		})
	}
}

type blockingAnswerWorkspace struct {
	recordingWorkspaceProvider
	entered chan struct{}
	release chan struct{}
}

func (p *blockingAnswerWorkspace) FinalizeWorkspace(ctx context.Context, req workspace.FinalizeRequest) (*workspace.FinalizeResult, error) {
	close(p.entered)
	select {
	case <-p.release:
		return &workspace.FinalizeResult{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestAnswerDeliveryPrecedesSlowWorkspaceFinalization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mem := store.NewMemory()
	agent := testAgent("app-a")
	agent.ExecutionConfig = json.RawMessage(`{"workspace":{"mode":"host_prepared"}}`)
	if err := mem.CreateAgent(ctx, &agent); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{ID: "slow-cleanup", AppID: agent.AppID, AgentID: agent.ID, Target: agentcore.TargetRef{Type: "ticket", ID: "T-1"}, RuntimeKind: agentcore.RuntimeNativeSDK, ExecutionMode: ExecutionModeLightweight, Status: agentcore.RunStatusQueued, Input: agentcore.RunInput{TurnPolicy: agentcore.TurnPolicy{Mode: agentcore.TurnPolicyPauseAfterAssist, CompletionMode: agentcore.TurnCompletionExplicit}}}
	if err := mem.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	provider := &blockingAnswerWorkspace{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(provider.release)
	provider.lease = agentcore.WorkspaceLease{ID: "lease", RootPath: t.TempDir(), CleanupPolicy: workspace.CleanupManual}
	workspaces := workspace.NewRegistry()
	if err := workspaces.Register(agent.AppID, provider); err != nil {
		t.Fatal(err)
	}
	events := &recordingEngineEventSink{}
	eng := New(Config{Store: mem, EventSink: events, Workspaces: workspaces, Targets: host.NewStaticContextProvider(), Tools: tools.NewRegistry(), Runtimes: runtime.NewRegistry(runtime.NewNativeAdapterWithConfig(runtime.NativeConfig{ModelFactory: answerModel{}}))})
	finished := make(chan error, 1)
	go func() { _, err := eng.ExecuteRunOnce(ctx, run.AppID, run.ID); finished <- err }()
	select {
	case <-provider.entered:
	case err := <-finished:
		t.Fatalf("finished before finalizer: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	saved, err := mem.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != agentcore.RunStatusRunning {
		t.Fatalf("run status prematurely changed: %s", saved.Status)
	}
	var final *Event
	for _, event := range events.snapshot() {
		if event.Type == "assistant_message_completed" && event.Data["message_type"] == "assistant_final" {
			copy := event
			final = &copy
		}
	}
	if final == nil || final.Data["content"] != engineAnswer || final.Data["turn_id"] == "" || final.Data["answer_completed_at"] == nil {
		t.Fatalf("answer unavailable during cleanup: %+v", final)
	}
	messages, err := mem.ListMessages(ctx, run.AppID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		if message.MessageType == "assistant_final" && message.Content == engineAnswer {
			found = true
		}
	}
	if !found {
		t.Fatal("final event preceded durable answer")
	}
	// Release without sleeping: the blocked finalizer represents arbitrary cleanup delay.
	provider.release <- struct{}{}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
