package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

// PushRepository publishes the primary checkout through the host repository
// provider so it can refresh its own credentials. Any lease the provider did
// not prepare (a local CLI checkout, a host-prepared http lease, a dynamically
// attached secondary checkout) reports ErrDirectPublicationUnsupported so the
// tool layer falls back to the credentials already present in the checkout.
func (m engineWorkspaceManager) PushRepository(ctx context.Context, message string) (json.RawMessage, error) {
	if m.run == nil || m.run.WorkspaceLease == nil {
		return nil, fmt.Errorf("repository checkout is required")
	}
	lease := m.run.WorkspaceLease
	if m.engine == nil || m.engine.cfg.Workspaces == nil || m.agent == nil || lease.Provider != "repository" {
		return nil, workspace.ErrDirectPublicationUnsupported
	}
	provider, ok := m.engine.cfg.Workspaces.Provider(m.run.AppID)
	if !ok {
		return nil, workspace.ErrDirectPublicationUnsupported
	}
	publisher, ok := provider.(interface {
		PushWorkspace(context.Context, workspace.PrepareRequest, string, string) (json.RawMessage, error)
	})
	if !ok {
		return nil, workspace.ErrDirectPublicationUnsupported
	}
	return publisher.PushWorkspace(ctx, workspace.PrepareRequest{AppID: m.run.AppID, RunID: m.run.ID, AgentID: m.run.AgentID, RuntimeKind: m.run.RuntimeKind, Target: m.run.Target, Metadata: m.run.Input.Metadata, WorkspaceMode: workspace.ModeRepository, ExecutionConfig: m.agent.ExecutionConfig}, lease.RootPath, message)
}
