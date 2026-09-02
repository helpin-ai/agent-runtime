package host

import (
	"context"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type ConfiguredAppAdapter struct {
	ID              string
	ContextProvider RunTargetContextProvider
	Register        func(ctx context.Context, registry *tools.Registry) error
}

func (a ConfiguredAppAdapter) AppID() string {
	return a.ID
}

func (a ConfiguredAppAdapter) ResolveTarget(ctx context.Context, appID string, target agentcore.TargetRef) (*TargetContext, error) {
	return a.ResolveRunTarget(ctx, TargetContextRequest{AppID: appID, Target: target})
}

func (a ConfiguredAppAdapter) ResolveRunTarget(ctx context.Context, req TargetContextRequest) (*TargetContext, error) {
	if a.ContextProvider != nil {
		return a.ContextProvider.ResolveRunTarget(ctx, req)
	}
	return NewStaticContextProvider().ResolveTarget(ctx, req.AppID, req.Target)
}

func (a ConfiguredAppAdapter) RegisterTools(ctx context.Context, registry *tools.Registry) error {
	if a.Register != nil {
		return a.Register(ctx, registry)
	}
	return nil
}
