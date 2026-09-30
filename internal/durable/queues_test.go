package durable

import "testing"

func TestCodingWorkerDoesNotPollSupportQueues(t *testing.T) {
	for _, q := range WorkerQueues(false) {
		if q.Name == TaskQueueName(QueueAgentNativeCoding) {
			t.Fatal("support serves coding")
		}
	}
	queues := WorkerQueues(true)
	if len(queues) != 1 || queues[0].Name != TaskQueueName(QueueAgentNativeCoding) {
		t.Fatalf("coding queues=%v", queues)
	}
	if queues[0].Concurrency != 4 {
		t.Fatalf("coding concurrency=%d, want 4", queues[0].Concurrency)
	}
}

func TestQueueForRuntimeUsesTaskQueuePrefix(t *testing.T) {
	t.Setenv("TEMPORAL_TASK_QUEUE_PREFIX", "usermaven-")

	got := QueueForRuntime("native_sdk", "interactive")

	if got != "usermaven-agent-native-interactive" {
		t.Fatalf("unexpected queue %q", got)
	}
}

func TestSharedQueuesUsesTaskQueuePrefix(t *testing.T) {
	t.Setenv("TEMPORAL_TASK_QUEUE_PREFIX", "usermaven-")

	queues := SharedQueues()
	if len(queues) == 0 {
		t.Fatal("expected shared queues")
	}
	for _, queue := range queues {
		if queue.Name == "" {
			t.Fatal("expected queue name")
		}
		if queue.Name[:len("usermaven-")] != "usermaven-" {
			t.Fatalf("expected prefixed queue name, got %q", queue.Name)
		}
	}
}

func TestA2ARunsUseTheirOwnSharedQueue(t *testing.T) {
	t.Setenv("TEMPORAL_TASK_QUEUE_PREFIX", "")

	if got := QueueForRuntime("a2a", "autonomous"); got != QueueAgentA2A {
		t.Fatalf("a2a runs routed to %q", got)
	}
	if got := QueueForRuntime("unknown", "autonomous"); got != QueueAutomation {
		t.Fatalf("other runtimes routed to %q", got)
	}
	for _, role := range []WorkerRole{WorkerRoleShared, WorkerRoleAll} {
		found := false
		for _, queue := range WorkerQueuesForRole(role) {
			if queue.Name == QueueAgentA2A {
				found = queue.Concurrency == 32
			}
		}
		if !found {
			t.Fatalf("%s workers do not poll %s with concurrency 32", role, QueueAgentA2A)
		}
	}
	for _, queue := range WorkerQueuesForRole(WorkerRoleExecution) {
		if queue.Name == QueueAgentA2A {
			t.Fatal("coding workers poll the a2a queue")
		}
	}
}

func TestQueueForRuntimePreservesDefaultNamesWithoutPrefix(t *testing.T) {
	t.Setenv("TEMPORAL_TASK_QUEUE_PREFIX", "")

	got := QueueForRuntime("native_sdk", "interactive")

	if got != QueueAgentNativeInteractive {
		t.Fatalf("unexpected queue %q", got)
	}
}
