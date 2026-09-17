package durable

import (
	"os"
	"strings"
)

const (
	QueueAgentNativeInteractive = "agent-native-interactive"
	QueueAgentNativeAutonomous  = "agent-native-autonomous"
	QueueAgentNativeCoding      = "agent-native-coding"
	QueueAutomation             = "automation-default"

	WorkflowSignalResume  = "ResumeRun"
	WorkflowSignalApprove = "ApproveRun"
	WorkflowSignalHandoff = "HandoffRun"
	WorkflowSignalMessage = "RunMessage"
)

// QueueConfig limits are per worker process; shared capacity scales with replicas.
type QueueConfig struct {
	Name        string
	Concurrency int
}

func SharedQueues() []QueueConfig {
	return []QueueConfig{
		{Name: TaskQueueName(QueueAgentNativeInteractive), Concurrency: 8},
		{Name: TaskQueueName(QueueAgentNativeAutonomous), Concurrency: 6},
		{Name: TaskQueueName(QueueAutomation), Concurrency: 4},
	}
}

// WorkerQueues keeps coding workers separate from the default shared workloads.
func WorkerQueues(coding bool) []QueueConfig {
	if coding {
		return []QueueConfig{{Name: TaskQueueName(QueueAgentNativeCoding), Concurrency: 50}}
	}
	return SharedQueues()
}

func QueueForRuntime(runtimeKind, invocationMode string) string {
	var queue string
	switch runtimeKind {
	case "native_sdk":
		if invocationMode == "interactive" {
			queue = QueueAgentNativeInteractive
		} else {
			queue = QueueAgentNativeAutonomous
		}
	default:
		queue = QueueAutomation
	}
	return TaskQueueName(queue)
}

func TaskQueueName(baseName string) string {
	baseName = strings.TrimSpace(baseName)
	if baseName == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv("TEMPORAL_TASK_QUEUE_PREFIX")) + baseName
}

func WorkflowIDForRun(runID string) string {
	return "agent-run-" + runID
}
