import { createFileRoute } from '@tanstack/react-router'
import { useSuspenseQuery } from '@tanstack/react-query'
import { AgentForm } from '~/components/agent-form'
import { agentQuery, systemConfigQuery } from '~/lib/queries'
import { useAppScope } from '~/lib/app-scope'

export const Route = createFileRoute('/agents/$agentId/edit')({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(agentQuery(context.appId, params.agentId)),
      context.queryClient.ensureQueryData(systemConfigQuery(context.appId)),
    ])
  },
  component: EditAgent,
})

function EditAgent() {
  const { agentId } = Route.useParams()
  const { appId } = useAppScope()
  const { data: agent } = useSuspenseQuery(agentQuery(appId, agentId))
  const { data: config } = useSuspenseQuery(systemConfigQuery(appId))
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Edit agent</h1>
        <p className="text-muted-foreground font-mono text-xs">{agent.id}</p>
      </div>
      <AgentForm
        appId={config.connection.app_id}
        initial={agent}
        capabilities={config.capabilities}
      />
    </div>
  )
}
