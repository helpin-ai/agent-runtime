package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/a2aproject/a2a-go/v2/a2a"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"time"
)

func a2aSupportsSteering(card *a2a.AgentCard) bool {
	if card == nil {
		return false
	}
	for _, extension := range card.Capabilities.Extensions {
		if extension.URI == sdk.A2ASteeringExtension {
			return true
		}
	}
	return false
}

// The remote extension must deduplicate command IDs. Only its explicit matching
// receipt acknowledges application; a successful send or task progress does not.
func (s *a2aSession) deliverSteering(ctx context.Context) error {
	store, ok := s.execCtx.Store.(agentcore.SteeringStore)
	if !ok || !s.state.SteeringSupported || s.state.TaskID == "" {
		return nil
	}
	if time.Now().Before(s.steeringNext) {
		return nil
	}
	s.steeringNext = time.Now().Add(s.adapter.pollMax)
	commands, err := store.ListRunSteering(ctx, s.execCtx.AppID, s.execCtx.Run.ID)
	if err != nil {
		return err
	}
	for _, command := range commands {
		if command.State != "accepted" {
			continue
		}
		message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{"extension": sdk.A2ASteeringExtension, "command": command}))
		message.ID = "helpin-steering-" + command.CommandID
		message.TaskID = a2a.TaskID(s.state.TaskID)
		message.ContextID = s.state.ContextID
		message.Extensions = []string{sdk.A2ASteeringExtension}
		callCtx, cancel := context.WithTimeout(ctx, a2aRequestTimeout)
		result, err := s.client.SendMessage(callCtx, &a2a.SendMessageRequest{Message: message, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
		cancel()
		if err != nil {
			return fmt.Errorf("send steering to external agent: %w", s.conn.redactErr(err))
		}
		var parts a2a.ContentParts
		switch response := result.(type) {
		case *a2a.Message:
			if response.TaskID != message.TaskID || response.ContextID != message.ContextID {
				return nil
			}
			parts = response.Parts
		case *a2a.Task:
			if response.ID != message.TaskID || response.ContextID != message.ContextID {
				return nil
			}
			if response.Status.Message != nil {
				parts = response.Status.Message.Parts
			}
		}
		state := a2aSteeringReceipt(parts, command)
		switch state {
		case "applied":
			err = store.ApplyRunSteering(ctx, s.execCtx.AppID, s.execCtx.Run.ID, command.Revision)
		case "rejected":
			err = store.RejectRunSteering(ctx, s.execCtx.AppID, s.execCtx.Run.ID, command.Revision)
		default:
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func a2aSteeringReceipt(parts a2a.ContentParts, command sdk.RunSteeringCommand) string {
	for _, part := range parts {
		if part == nil {
			continue
		}
		data, ok := part.Content.(a2a.Data)
		if !ok {
			continue
		}
		raw, err := json.Marshal(data.Value)
		if err != nil {
			continue
		}
		var receipt struct {
			Extension string `json:"extension"`
			CommandID string `json:"command_id"`
			Revision  int64  `json:"revision"`
			State     string `json:"state"`
		}
		if json.Unmarshal(raw, &receipt) == nil && receipt.Extension == sdk.A2ASteeringExtension && receipt.CommandID == command.CommandID && receipt.Revision == command.Revision {
			return receipt.State
		}
	}
	return ""
}

// Non-blocking A2A sends may return before the receipt is emitted. Reconcile
// the same structured acknowledgement from subsequent task observations.
func (s *a2aSession) observeSteering(ctx context.Context, task *a2a.Task) error {
	if !s.state.SteeringSupported || string(task.ID) != s.state.TaskID || task.ContextID != s.state.ContextID {
		return nil
	}
	store, ok := s.execCtx.Store.(agentcore.SteeringStore)
	if !ok {
		return nil
	}
	commands, err := store.ListRunSteering(ctx, s.execCtx.AppID, s.execCtx.Run.ID)
	if err != nil {
		return err
	}
	var parts a2a.ContentParts
	if task.Status.Message != nil {
		parts = append(parts, task.Status.Message.Parts...)
	}
	for _, message := range task.History {
		if message != nil {
			parts = append(parts, message.Parts...)
		}
	}
	for _, artifact := range task.Artifacts {
		if artifact != nil {
			parts = append(parts, artifact.Parts...)
		}
	}
	for _, command := range commands {
		if command.State != "accepted" {
			continue
		}
		switch a2aSteeringReceipt(parts, command) {
		case "applied":
			err = store.ApplyRunSteering(ctx, s.execCtx.AppID, s.execCtx.Run.ID, command.Revision)
		case "rejected":
			err = store.RejectRunSteering(ctx, s.execCtx.AppID, s.execCtx.Run.ID, command.Revision)
		default:
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
