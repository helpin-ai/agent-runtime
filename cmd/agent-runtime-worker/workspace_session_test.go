package main

import (
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/durable"
	"github.com/helpin-ai/agent-runtime/internal/workspace"
)

func TestSessionWorkerOptionsRetainCommandConcurrency(t *testing.T) {
	t.Setenv(workspace.StorageModeEnv, "ephemeral")
	options := workerOptions(durable.QueueConfig{Name: durable.TaskQueueName(durable.QueueAgentNativeCoding), Concurrency: 4})
	if !options.EnableSessionWorker || options.MaxConcurrentActivityExecutionSize != 4 || options.MaxConcurrentSessionExecutionSize != 256 {
		t.Fatal("session affinity changed coding command capacity")
	}
	if workerOptions(durable.QueueConfig{Name: "shared", Concurrency: 8}).EnableSessionWorker {
		t.Fatal("shared worker enabled workspace sessions")
	}
}
