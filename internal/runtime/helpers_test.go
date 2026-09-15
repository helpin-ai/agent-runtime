package runtime

import (
	"context"
	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

type testEventSink struct {
	events []Event
}

func (s *testEventSink) Emit(ctx context.Context, event Event) {
	s.events = append(s.events, event)
}

func (s *testEventSink) hasType(eventType string) bool {
	for _, event := range s.events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

type testArtifactWriter struct {
	store agentcore.Store
	run   *agentcore.AgentRun
}

func (w testArtifactWriter) WriteArtifact(ctx context.Context, artifact agentcore.AgentRunArtifact) error {
	artifact.AppID = w.run.AppID
	artifact.RunID = w.run.ID
	return w.store.AppendArtifact(ctx, &artifact)
}
