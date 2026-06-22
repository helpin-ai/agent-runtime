package durable

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
		{Name: QueueAgentNativeInteractive, Concurrency: 8},
		{Name: QueueAgentNativeAutonomous, Concurrency: 6},
		{Name: QueueAgentCodexAutonomous, Concurrency: 4},
		{Name: QueueAgentCodexInteractive, Concurrency: 4},
		{Name: QueueAgentOpenCodeAutonomous, Concurrency: 4},
		{Name: QueueAgentOpenCodeInteractive, Concurrency: 4},
		{Name: QueueAutomation, Concurrency: 4},
	}
}

func QueueForRuntime(runtimeKind, invocationMode string) string {
	switch runtimeKind {
	case "native_sdk":
		if invocationMode == "interactive" {
			return QueueAgentNativeInteractive
		}
		return QueueAgentNativeAutonomous
	case "codex":
		if invocationMode == "interactive" {
			return QueueAgentCodexInteractive
		}
		return QueueAgentCodexAutonomous
	case "opencode":
		if invocationMode == "interactive" {
			return QueueAgentOpenCodeInteractive
		}
		return QueueAgentOpenCodeAutonomous
	default:
		return QueueAutomation
	}
}

func WorkflowIDForRun(runID string) string {
	return "agent-run-" + runID
}
