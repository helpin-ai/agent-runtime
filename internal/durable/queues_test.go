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

func TestQueueForRuntimePreservesDefaultNamesWithoutPrefix(t *testing.T) {
	t.Setenv("TEMPORAL_TASK_QUEUE_PREFIX", "")

	got := QueueForRuntime("native_sdk", "interactive")

	if got != QueueAgentNativeInteractive {
		t.Fatalf("unexpected queue %q", got)
	}
}
