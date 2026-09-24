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
	WorkflowSignalPause   = "PauseRun"
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

// WorkerRole selects which task queues one worker process polls.
type WorkerRole string

const (
	// WorkerRoleShared polls the interactive, autonomous and automation queues.
	WorkerRoleShared WorkerRole = "shared"
	// WorkerRoleExecution polls only the execution (coding) queue.
	WorkerRoleExecution WorkerRole = "execution"
	// WorkerRoleAll polls every queue from one process. Intended for
	// single-tenant installs that accept shared execution and chat workloads.
	WorkerRoleAll WorkerRole = "all"
)

// ExecutionQueues serves the execution lane. Landlock confines every spawned
// command to its run directory, while the run-level workspace lock fences
// retries that land on another replica. Keep this bounded because concurrent
// builds still share the pod's CPU and memory cgroup.
func ExecutionQueues() []QueueConfig {
	return []QueueConfig{{Name: TaskQueueName(QueueAgentNativeCoding), Concurrency: 4}}
}

// WorkerQueuesForRole returns the queues a worker process polls for its role.
func WorkerQueuesForRole(role WorkerRole) []QueueConfig {
	switch role {
	case WorkerRoleExecution:
		return ExecutionQueues()
	case WorkerRoleAll:
		return append(SharedQueues(), ExecutionQueues()...)
	default:
		return SharedQueues()
	}
}

// WorkerQueues keeps execution workers separate from the default shared workloads.
func WorkerQueues(coding bool) []QueueConfig {
	if coding {
		return WorkerQueuesForRole(WorkerRoleExecution)
	}
	return WorkerQueuesForRole(WorkerRoleShared)
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
