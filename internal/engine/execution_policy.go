package engine

import (
	"context"
	"errors"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// ErrRetiredRuntime is also used for historical runs whose executor is gone.
var ErrRetiredRuntime = errors.New("This run used a retired coding engine and cannot continue. Start a new run using native_sdk; review existing changes and completed actions before retrying.")

// ErrCodingUnavailable prevents coding work from entering shared support queues.
var ErrCodingUnavailable = errors.New("This tool policy requires an isolated coding worker. Configure a coding worker or remove shell and workspace-write tools before starting a new run.")

// CodingMetadataKey is persisted only after admission resolves effective tools.
const CodingMetadataKey = "native_coding"

func retiredRuntime(kind string) bool { return kind == "codex" || kind == "opencode" }

func (e *Engine) admitTools(ctx context.Context, agent *agentcore.Agent, requested []string, mode string) error {
	if retiredRuntime(agent.RuntimeKind) {
		return ErrRetiredRuntime
	}
	if !tools.RequiresCoding(tools.AllowedSet(agent, requested)) {
		return nil
	}
	if mode == ExecutionModeLightweight {
		if e.cfg.CodingWorker {
			return nil
		}
		return ErrCodingUnavailable
	}
	if e.cfg.CheckCodingAdmission == nil {
		return ErrCodingUnavailable
	}
	return e.cfg.CheckCodingAdmission(ctx)
}

func (e *Engine) executionPolicy(agent *agentcore.Agent, run *agentcore.AgentRun) error {
	if retiredRuntime(run.RuntimeKind) {
		return ErrRetiredRuntime
	}
	if tools.RequiresCoding(tools.AllowedSet(agent, run.Input.AllowedTools)) && !e.cfg.CodingWorker {
		return ErrCodingUnavailable
	}
	return nil
}

func (e *Engine) admitExistingRun(ctx context.Context, run *agentcore.AgentRun) error {
	if retiredRuntime(run.RuntimeKind) {
		return ErrRetiredRuntime
	}
	agent, err := e.cfg.Store.GetAgent(ctx, run.AppID, run.AgentID)
	if err != nil {
		return err
	}
	if agent == nil {
		return errors.New("agent not found")
	}
	if tools.RequiresCoding(tools.AllowedSet(agent, run.Input.AllowedTools)) && run.ExecutionMode == ExecutionModeDurable {
		coding, _ := run.Input.Metadata[CodingMetadataKey].(bool)
		if !coding {
			return errors.New("This run was started on a support queue before its tools required coding execution. Start a new run on the coding worker; review completed actions before retrying.")
		}
	}
	return e.admitTools(ctx, agent, run.Input.AllowedTools, run.ExecutionMode)
}
