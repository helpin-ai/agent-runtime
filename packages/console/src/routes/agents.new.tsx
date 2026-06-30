import { createFileRoute } from '@tanstack/react-router'
import { useSuspenseQuery } from '@tanstack/react-query'
import { AgentForm } from '~/components/agent-form'
import { systemConfigQuery } from '~/lib/queries'

export const Route = createFileRoute('/agents/new')({
  loader: ({ context }) =>
    context.queryClient.ensureQueryData(systemConfigQuery()),
  component: NewAgent,
})

function NewAgent() {
  const { data: config } = useSuspenseQuery(systemConfigQuery())
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">New agent</h1>
        <p className="text-muted-foreground text-sm">
          Register an agent for app{' '}
          <span className="font-mono">{config.connection.app_id}</span>.
        </p>
      </div>
      <AgentForm
        appId={config.connection.app_id}
        capabilities={config.capabilities}
      />
    </div>
  )
}
