package engine

import (
	"context"
	"encoding/json"
	"errors"

	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/modelauth"
)

func (e *Engine) UpdateRunModelCredential(ctx context.Context, appID, runID string, c sdk.ModelCredential) (*sdk.RunModelCredentialUpdate, error) {
	run, err := e.requireRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if agentcore.IsTerminalStatus(run.Status) {
		return nil, errors.New("agent run is terminal")
	}
	if e.cfg.ModelCredentials == nil {
		return nil, errors.New("run model credentials are not configured")
	}
	if err := e.cfg.ModelCredentials.Replace(ctx, appID, runID, c); err != nil {
		return nil, err
	}
	return &sdk.RunModelCredentialUpdate{RunID: runID, ExpiresAt: c.ExpiresAt}, nil
}
func (e *Engine) RevokeRunModelCredential(ctx context.Context, appID, runID string) error {
	if _, err := e.requireRun(ctx, appID, runID); err != nil {
		return err
	}
	if e.cfg.ModelCredentials == nil {
		return errors.New("run model credentials are not configured")
	}
	return e.cfg.ModelCredentials.Store.ClearRunModelCredential(ctx, appID, runID)
}
func (e *Engine) pauseForModelAuthentication(ctx context.Context, run *agentcore.AgentRun, authErr *modelauth.AuthenticationError) error {
	payload, _ := json.Marshal(map[string]string{"provider": authErr.Provider, "connection_id": authErr.ConnectionID, "reason": authErr.Reason})
	if err := e.cfg.Store.AppendInteraction(ctx, &agentcore.AgentRunInteraction{
		AppID: run.AppID, RunID: run.ID, RuntimeKind: run.RuntimeKind, InteractionKind: "authentication", Status: "pending",
		Title: "AI connection requires authentication", Summary: "Reconnect your AI connection to continue this run.", RequestPayload: payload,
	}); err != nil {
		return err
	}
	run.Status = agentcore.RunStatusPaused
	run.PauseReason = agentcore.PauseReasonAuth
	run.ErrorMessage = ""
	run.CompletedAt = nil
	if err := e.cfg.Store.UpdateRun(ctx, run); err != nil {
		return err
	}
	e.emitRunEvent(ctx, run, "run.paused", map[string]any{"pause_reason": run.PauseReason, "provider": authErr.Provider, "connection_id": authErr.ConnectionID})
	return nil
}
