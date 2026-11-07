package runtime

import (
	"context"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
	"strings"
	"testing"
)

func TestSteeringCheckpointSurvivesRestartWithoutDuplicateInstruction(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	run := &agentcore.AgentRun{ID: "r", AppID: "a", RuntimeKind: agentcore.RuntimeNativeSDK, Status: agentcore.RunStatusRunning}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	command, err := db.CreateRunSteering(ctx, "a", "r", sdk.SteerRunRequest{CommandID: "c", Content: "Research only", ExternalActorID: "user", DeniedTools: []string{"crm_write"}})
	if err != nil {
		t.Fatal(err)
	}
	ec := &ExecutionContext{Context: ctx, Store: db, AppID: "a", Run: run}
	recorder, err := openNativeRecorder(ctx, ec, false)
	if err != nil {
		t.Fatal(err)
	}
	result := &nativeExecutionResult{}
	if err := applyNativeSteering(ctx, recorder, result); err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 || !strings.Contains(result.Messages[0].Content, "Research only") {
		t.Fatalf("instructions: %+v", result.Messages)
	}
	restarted, err := openNativeRecorder(ctx, ec, false)
	if err != nil {
		t.Fatal(err)
	}
	result = &nativeExecutionResult{Messages: restarted.state.Messages}
	if err := applyNativeSteering(ctx, restarted, result); err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("duplicate instructions: %+v", result.Messages)
	}
	commands, err := db.ListRunSteering(ctx, "a", "r")
	if err != nil || commands[0].State != "applied" || commands[0].Revision != command.Revision {
		t.Fatalf("receipt: %+v %v", commands, err)
	}
	if err := nativeSteeringAllowsTool(ctx, ec, "crm_write"); err == nil {
		t.Fatal("disabled tool allowed")
	}
	if err := nativeSteeringAllowsTool(ctx, ec, "crm_read"); err != nil {
		t.Fatal(err)
	}
}

type steeringDuringModel struct {
	db    *store.Memory
	calls int
	t     *testing.T
}

func (m *steeringDuringModel) Generate(ctx context.Context, req NativeModelRequest) (*NativeModelResponse, error) {
	m.calls++
	if m.calls == 1 {
		_, err := m.db.CreateRunSteering(ctx, "a", "r", sdk.SteerRunRequest{CommandID: "late", Content: "Use the revised deliverable", ExternalActorID: "user"})
		if err != nil {
			m.t.Fatal(err)
		}
	} else {
		found := false
		for _, message := range req.Messages {
			if strings.Contains(message.Content, "revised deliverable") {
				found = true
			}
		}
		if !found {
			m.t.Fatal("late instruction never reached the model")
		}
	}
	return &NativeModelResponse{Message: NativeMessage{Role: "assistant", Content: "Done"}}, nil
}

func TestSteeringArrivingDuringFinalModelResponseGetsAnotherTurn(t *testing.T) {
	ctx := context.Background()
	db := store.NewMemory()
	run := &agentcore.AgentRun{ID: "r", AppID: "a", RuntimeKind: agentcore.RuntimeNativeSDK, Status: agentcore.RunStatusRunning}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	model := &steeringDuringModel{db: db, t: t}
	adapter := NewNativeAdapterWithConfig(NativeConfig{ModelFactory: fakeNativeFactory{model: model}, MaxToolSteps: 4})
	_, err := adapter.Execute(&ExecutionContext{Context: ctx, AppID: "a", Store: db, Run: run, Agent: &agentcore.Agent{RuntimeKind: agentcore.RuntimeNativeSDK}})
	if err != nil || model.calls != 2 {
		t.Fatalf("calls=%d err=%v", model.calls, err)
	}
	commands, err := db.ListRunSteering(ctx, "a", "r")
	if err != nil || len(commands) != 1 || commands[0].State != "applied" {
		t.Fatalf("receipt: %+v %v", commands, err)
	}
}
