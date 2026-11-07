package runtime

import (
	"context"
	"github.com/a2aproject/a2a-go/v2/a2a"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"testing"
)

func TestA2ASteeringRequiresDeclaredExtensionAndExactReceipt(t *testing.T) {
	card := &a2a.AgentCard{}
	if a2aSupportsSteering(card) {
		t.Fatal("undeclared extension accepted")
	}
	card.Capabilities.Extensions = []a2a.AgentExtension{{URI: sdk.A2ASteeringExtension}}
	if !a2aSupportsSteering(card) {
		t.Fatal("declared extension missing")
	}
	command := sdk.RunSteeringCommand{SteerRunRequest: sdk.SteerRunRequest{CommandID: "command"}, Revision: 3}
	for _, test := range []struct {
		name, id, extension, state string
		revision                   int64
		want                       string
	}{
		{"matching", "command", sdk.A2ASteeringExtension, "applied", 3, "applied"},
		{"rejected", "command", sdk.A2ASteeringExtension, "rejected", 3, "rejected"},
		{"wrong command", "other", sdk.A2ASteeringExtension, "applied", 3, ""},
		{"wrong revision", "command", sdk.A2ASteeringExtension, "applied", 2, ""},
		{"wrong version", "command", "steering/v2", "applied", 3, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			parts := a2a.ContentParts{a2a.NewTextPart("applied"), a2a.NewDataPart(map[string]any{"extension": test.extension, "command_id": test.id, "revision": test.revision, "state": test.state})}
			if got := a2aSteeringReceipt(parts, command); got != test.want {
				t.Fatalf("receipt = %q", got)
			}
		})
	}
	if got := a2aSteeringReceipt(a2a.ContentParts{a2a.NewTextPart("applied")}, command); got != "" {
		t.Fatal("prose acknowledged command")
	}
}

func TestA2ASteeringAcknowledgementCanArriveOnLaterTaskPoll(t *testing.T) {
	h := newA2AHarness(t)
	ctx := context.Background()
	command, err := h.mem.CreateRunSteering(ctx, "app", "run-1", sdk.SteerRunRequest{CommandID: "late-receipt", Content: "Research only", ExternalActorID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	session := &a2aSession{execCtx: h.execCtx(ctx, nil), state: a2aRunState{SteeringSupported: true, TaskID: "task", ContextID: "context"}}
	receipt := a2a.NewDataPart(map[string]any{"extension": sdk.A2ASteeringExtension, "command_id": command.CommandID, "revision": command.Revision, "state": "applied"})
	task := &a2a.Task{ID: "wrong-task", ContextID: "context", Status: a2a.TaskStatus{Message: a2a.NewMessage(a2a.MessageRoleAgent, receipt)}}
	if err := session.observeSteering(ctx, task); err != nil {
		t.Fatal(err)
	}
	commands, _ := h.mem.ListRunSteering(ctx, "app", "run-1")
	if commands[0].State != "accepted" {
		t.Fatal("foreign task acknowledged steering")
	}
	task.ID = "task"
	if err := session.observeSteering(ctx, task); err != nil {
		t.Fatal(err)
	}
	commands, _ = h.mem.ListRunSteering(ctx, "app", "run-1")
	if commands[0].State != "applied" {
		t.Fatal("later acknowledgement was lost")
	}
}
