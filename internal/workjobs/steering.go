package workjobs

import (
	"context"
	"fmt"
	sdk "github.com/helpin-ai/agent-runtime-go"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

// The specification is the durable fan-out record. Reconciliation retries a
// missed worker after restart; the revision-derived ID deduplicates delivery.
func (s *Service) steerWorker(ctx context.Context, job *agentcore.WorkJob, run *agentcore.AgentRun) error {
	if job.Spec.Steering == nil {
		return nil
	}
	commands, err := s.cfg.Engine.RunSteering(ctx, job.AppID, run.ID)
	if err != nil {
		return err
	}
	commandID := fmt.Sprintf("job:%s:spec:%d", job.ID, job.SpecRevision)
	var revision int64
	for _, command := range commands {
		if command.CommandID == commandID {
			return nil
		}
		revision = command.Revision
	}
	_, err = s.cfg.Engine.SteerRun(ctx, job.AppID, run.ID, sdk.SteerRunRequest{
		CommandID: commandID, ExpectedRevision: revision,
		Content: job.Spec.Steering.Content, DeniedTools: append([]string(nil), job.Spec.Steering.DeniedTools...), ExternalActorID: "job:" + job.ID,
	})
	return err
}
