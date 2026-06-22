package host

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

type AppAdapter interface {
	AppID() string
}

type TargetResolver interface {
	ResolveTarget(ctx context.Context, appID string, target agentcore.TargetRef) (*TargetContext, error)
}

type ToolRegistrar interface {
	RegisterTools(ctx context.Context, registry *tools.Registry) error
}

type AdapterRegistry struct {
	mu       sync.RWMutex
	adapters map[string]AppAdapter
	fallback TargetContextProvider
}

func NewAdapterRegistry(fallback TargetContextProvider) *AdapterRegistry {
	if fallback == nil {
		fallback = NewStaticContextProvider()
	}
	return &AdapterRegistry{
		adapters: map[string]AppAdapter{},
		fallback: fallback,
	}
}

func (r *AdapterRegistry) Register(ctx context.Context, adapter AppAdapter, registry *tools.Registry) error {
	if r == nil {
		return fmt.Errorf("adapter registry is not configured")
	}
	if adapter == nil {
		return fmt.Errorf("adapter is required")
	}
	appID := strings.TrimSpace(adapter.AppID())
	if appID == "" {
		return fmt.Errorf("adapter app_id is required")
	}
	if registrar, ok := adapter.(ToolRegistrar); ok {
		if err := registrar.RegisterTools(ctx, registry); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[appID] = adapter
	return nil
}

func (r *AdapterRegistry) ResolveTarget(ctx context.Context, appID string, target agentcore.TargetRef) (*TargetContext, error) {
	return r.ResolveRunTarget(ctx, TargetContextRequest{AppID: appID, Target: target})
}

func (r *AdapterRegistry) ResolveRunTarget(ctx context.Context, req TargetContextRequest) (*TargetContext, error) {
	if r == nil {
		return nil, fmt.Errorf("adapter registry is not configured")
	}
	appID := strings.TrimSpace(req.AppID)
	r.mu.RLock()
	adapter := r.adapters[appID]
	fallback := r.fallback
	r.mu.RUnlock()
	if resolver, ok := adapter.(RunTargetContextProvider); ok {
		return resolver.ResolveRunTarget(ctx, req)
	}
	if resolver, ok := adapter.(TargetResolver); ok {
		return resolver.ResolveTarget(ctx, appID, req.Target)
	}
	if resolver, ok := fallback.(RunTargetContextProvider); ok {
		return resolver.ResolveRunTarget(ctx, req)
	}
	return fallback.ResolveTarget(ctx, appID, req.Target)
}

func (r *AdapterRegistry) Adapter(appID string) (AppAdapter, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.adapters[strings.TrimSpace(appID)]
	return adapter, ok
}

var _ TargetContextProvider = (*AdapterRegistry)(nil)
var _ RunTargetContextProvider = (*AdapterRegistry)(nil)
