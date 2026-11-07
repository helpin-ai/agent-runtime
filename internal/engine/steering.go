package engine

import (
	"context"
	"fmt"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"sort"
	"strings"
)

func (e *Engine) SteerRun(ctx context.Context, appID, runID string, req sdk.SteerRunRequest) (*sdk.RunSteeringCommand, error) {
	run, err := e.requireRun(ctx, appID, runID)
	if err != nil {
		return nil, err
	}
	if err = e.admitExistingRun(ctx, run); err != nil {
		return nil, err
	}
	supported := run.RuntimeKind == agentcore.RuntimeNativeSDK
	if run.RuntimeKind == agentcore.RuntimeA2A {
		if state, ok := run.Input.Metadata["a2a"].(map[string]interface{}); ok {
			supported, _ = state["steering_supported"].(bool)
		}
	}
	if !supported {
		return nil, fmt.Errorf("runtime does not support acknowledged steering")
	}
	if req.CommandID == "" || len(req.CommandID) > 128 || strings.TrimSpace(req.ExternalActorID) == "" || req.ExpectedRevision < 0 || len(req.Content) > 32768 || len(req.DeniedTools) > 128 {
		return nil, fmt.Errorf("invalid steering request")
	}
	if strings.TrimSpace(req.Content) == "" && len(req.DeniedTools) == 0 {
		return nil, fmt.Errorf("steering requires instructions or tool restrictions")
	}
	for _, tool := range req.DeniedTools {
		if strings.TrimSpace(tool) == "" || len(tool) > 256 {
			return nil, fmt.Errorf("invalid denied tool")
		}
	}
	sort.Strings(req.DeniedTools)
	store, ok := e.cfg.Store.(agentcore.SteeringStore)
	if !ok {
		return nil, fmt.Errorf("steering storage is unavailable")
	}
	return store.CreateRunSteering(ctx, appID, runID, req)
}
func (e *Engine) RunSteering(ctx context.Context, appID, runID string) ([]sdk.RunSteeringCommand, error) {
	if _, err := e.requireRun(ctx, appID, runID); err != nil {
		return nil, err
	}
	store, ok := e.cfg.Store.(agentcore.SteeringStore)
	if !ok {
		return nil, fmt.Errorf("steering storage is unavailable")
	}
	return store.ListRunSteering(ctx, appID, runID)
}
