package durable

import (
	"os"
	"strings"
)

const (
	QueueAgentNativeInteractive   = "agent-native-interactive"
	QueueAgentNativeAutonomous    = "agent-native-autonomous"
	QueueAgentCodexAutonomous     = "agent-codex-autonomous"
	QueueAgentCodexInteractive    = "agent-codex-interactive"
	QueueAgentOpenCodeAutonomous  = "agent-opencode-autonomous"
	QueueAgentOpenCodeInteractive = "agent-opencode-interactive"
	QueueAutomation               = "automation-default"

	WorkflowSignalResume  = "ResumeRun"
	WorkflowSignalApprove = "ApproveRun"
	WorkflowSignalHandoff = "HandoffRun"
	WorkflowSignalMessage = "RunMessage"
)

type QueueConfig struct {
	Name        string
	Concurrency int
}

func SharedQueues() []QueueConfig {
	return []QueueConfig{
		{Name: TaskQueueName(QueueAgentNativeInteractive), Concurrency: 8},
		{Name: TaskQueueName(QueueAgentNativeAutonomous), Concurrency: 6},
		{Name: TaskQueueName(QueueAgentCodexAutonomous), Concurrency: 4},
		{Name: TaskQueueName(QueueAgentCodexInteractive), Concurrency: 4},
		{Name: TaskQueueName(QueueAgentOpenCodeAutonomous), Concurrency: 4},
		{Name: TaskQueueName(QueueAgentOpenCodeInteractive), Concurrency: 4},
		{Name: TaskQueueName(QueueAutomation), Concurrency: 4},
	}
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
	case "codex":
		if invocationMode == "interactive" {
			queue = QueueAgentCodexInteractive
		} else {
			queue = QueueAgentCodexAutonomous
		}
	case "opencode":
		if invocationMode == "interactive" {
			queue = QueueAgentOpenCodeInteractive
		} else {
			queue = QueueAgentOpenCodeAutonomous
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
