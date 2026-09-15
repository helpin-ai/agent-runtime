package engine

import (
	"context"
	"errors"
	sdk "github.com/helpin-ai/agent-runtime-go"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/tools"
)

// ErrRetiredRuntime is also used for historical runs whose executor is gone.
var ErrRetiredRuntime = errors.New("This run used a retired coding engine and cannot continue. Start a new run using native_sdk; review existing changes and completed actions before retrying.")

// ErrCodingUnavailable prevents coding work from entering the default shared queues.
var ErrCodingUnavailable = errors.New("This tool policy requires an isolated coding worker. Configure a coding worker or remove shell and workspace-write tools before starting a new run.")

// ErrRunModelCredentialsRequired rejects ambient credentials for opted-in apps.
var ErrRunModelCredentialsRequired = errors.New("This app requires an explicit run model and model credential. Configure an AI connection in the host app before starting the run.")

func (e *Engine) requiresRunModelCredentials(appID string) bool {
	return e.cfg.RequireRunModelCredentials != nil && e.cfg.RequireRunModelCredentials(appID)
}

func (e *Engine) admitStoredModelPolicy(run *agentcore.AgentRun) error {
	if e.requiresRunModelCredentials(run.AppID) && (run.Input.Model == nil || run.Input.CredentialSource != "app") {
		return ErrRunModelCredentialsRequired
	}
	return e.admitModelEndpoint(run.AppID, run.Input.Model)
}

func (e *Engine) admitModelEndpoint(appID string, model *sdk.RunModel) error {
	if model == nil || model.Provider != "openai_compatible" {
		return nil
	}
	if e.cfg.ValidateRunModelEndpoint == nil {
		return errors.New("compatible model endpoints are not configured")
	}
	return e.cfg.ValidateRunModelEndpoint(appID, model)
}

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
	if err := e.admitStoredModelPolicy(run); err != nil {
		return err
	}
	if retiredRuntime(run.RuntimeKind) {
		return ErrRetiredRuntime
	}
	if tools.RequiresCoding(tools.AllowedSet(agent, run.Input.AllowedTools)) && !e.cfg.CodingWorker {
		return ErrCodingUnavailable
	}
	return nil
}

func (e *Engine) admitExistingRun(ctx context.Context, run *agentcore.AgentRun) error {
	if err := e.admitStoredModelPolicy(run); err != nil {
		return err
	}
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
			return errors.New("This run was started on a default queue before its tools required coding execution. Start a new run on the coding worker; review completed actions before retrying.")
		}
	}
	return e.admitTools(ctx, agent, run.Input.AllowedTools, run.ExecutionMode)
}
