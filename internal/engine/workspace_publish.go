package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func (m engineWorkspaceManager) PushRepository(ctx context.Context, message string) (json.RawMessage, error) {
	if m.run == nil || m.run.WorkspaceLease == nil {
		return nil, fmt.Errorf("repository checkout is required")
	}
	provider, ok := m.engine.cfg.Workspaces.Provider(m.run.AppID)
	if !ok {
		return nil, fmt.Errorf("repository provider is unavailable")
	}
	publisher, ok := provider.(interface {
		PushWorkspace(context.Context, workspace.PrepareRequest, string, string) (json.RawMessage, error)
	})
	if !ok {
		return nil, fmt.Errorf("repository provider does not support direct publication")
	}
	lease := m.run.WorkspaceLease
	return publisher.PushWorkspace(ctx, workspace.PrepareRequest{AppID: m.run.AppID, RunID: m.run.ID, AgentID: m.run.AgentID, RuntimeKind: m.run.RuntimeKind, Target: m.run.Target, Metadata: m.run.Input.Metadata, WorkspaceMode: workspace.ModeRepository, ExecutionConfig: m.agent.ExecutionConfig}, lease.RootPath, message)
}
