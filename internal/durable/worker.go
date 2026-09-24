package durable

import (
	"go.temporal.io/sdk/activity"
	tworker "go.temporal.io/sdk/worker"
)

func RegisterAgentRunWorker(w tworker.Worker, activities *AgentRunActivities) {
	w.RegisterWorkflow(AgentRunWorkflow)
	if activities == nil {
		return
	}
	w.RegisterActivityWithOptions(activities.PrepareLocalRunActivity, activity.RegisterOptions{Name: "AgentRunActivities.PrepareLocalRunActivity"})
	w.RegisterActivityWithOptions(activities.ExecuteLocalRunActivity, activity.RegisterOptions{Name: "AgentRunActivities.ExecuteLocalRunActivity"})
	w.RegisterActivityWithOptions(activities.CleanupLocalWorkspaceActivity, activity.RegisterOptions{Name: "AgentRunActivities.CleanupLocalWorkspaceActivity"})
	w.RegisterActivityWithOptions(activities.PrepareRunActivity, activity.RegisterOptions{
		Name: "AgentRunActivities.PrepareRunActivity",
	})
	w.RegisterActivityWithOptions(activities.ExecuteRunActivity, activity.RegisterOptions{
		Name: "AgentRunActivities.ExecuteRunActivity",
	})
	w.RegisterActivityWithOptions(activities.MarkRunManuallyPausedActivity, activity.RegisterOptions{
		Name: "AgentRunActivities.MarkRunManuallyPausedActivity",
	})
	w.RegisterActivityWithOptions(activities.CleanupTerminalWorkspaceActivity, activity.RegisterOptions{Name: "AgentRunActivities.CleanupTerminalWorkspaceActivity"})
	w.RegisterActivityWithOptions(activities.MarkRunFailedActivity, activity.RegisterOptions{
		Name: "AgentRunActivities.MarkRunFailedActivity",
	})
}
