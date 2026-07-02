package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/sdk"
)

type TargetContext = sdk.TargetContext
type TargetContextRequest = sdk.TargetContextRequest

type TargetContextProvider interface {
	ResolveTarget(ctx context.Context, appID string, target agentcore.TargetRef) (*TargetContext, error)
}

type RunTargetContextProvider interface {
	ResolveRunTarget(ctx context.Context, req TargetContextRequest) (*TargetContext, error)
}

type StaticContextProvider struct {
	contexts map[string]TargetContext
}

func NewStaticContextProvider() *StaticContextProvider {
	return &StaticContextProvider{contexts: map[string]TargetContext{}}
}

func (p *StaticContextProvider) Register(appID string, target agentcore.TargetRef, targetContext TargetContext) {
	if p == nil {
		return
	}
	targetContext.Target = target
	p.contexts[contextKey(appID, target)] = targetContext
}

func (p *StaticContextProvider) ResolveTarget(_ context.Context, appID string, target agentcore.TargetRef) (*TargetContext, error) {
	if strings.TrimSpace(appID) == "" {
		return nil, fmt.Errorf("app_id is required")
	}
	if strings.TrimSpace(target.Type) == "" || strings.TrimSpace(target.ID) == "" {
		return nil, fmt.Errorf("target.type and target.id are required")
	}
	if p != nil {
		if found, ok := p.contexts[contextKey(appID, target)]; ok {
			cp := found
			return &cp, nil
		}
	}
	return &TargetContext{
		Target:  target,
		Summary: fmt.Sprintf("%s target %s", target.Type, target.ID),
		Data:    map[string]interface{}{},
	}, nil
}

func contextKey(appID string, target agentcore.TargetRef) string {
	return strings.TrimSpace(appID) + "/" + strings.TrimSpace(target.Type) + "/" + strings.TrimSpace(target.ID)
}
