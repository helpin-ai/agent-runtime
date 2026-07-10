import { createFileRoute, Link } from '@tanstack/react-router'
import { useSuspenseQuery } from '@tanstack/react-query'
import { AlertTriangle } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '~/components/ui/card'
import { agentQuery, systemConfigQuery } from '~/lib/queries'
import { agentEffectiveProvider } from '~/lib/health'
import { useAppScope } from '~/lib/app-scope'

export const Route = createFileRoute('/agents/$agentId')({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(agentQuery(context.appId, params.agentId)),
      context.queryClient.ensureQueryData(systemConfigQuery(context.appId)),
    ])
  },
  component: AgentDetail,
})

function AgentDetail() {
  const { agentId } = Route.useParams()
  const { appId } = useAppScope()
  const { data: agent } = useSuspenseQuery(agentQuery(appId, agentId))
  const { data: config } = useSuspenseQuery(systemConfigQuery(appId))

  const isNative = !agent.runtime_kind || agent.runtime_kind === 'native_sdk'
  const provider = agentEffectiveProvider(agent)
  const providerConfigured = config.capabilities.providers.some(
    (p) => p.name === provider && p.configured,
  )
  const providerWarning = isNative && !providerConfigured

  return (
    <div className="space-y-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <div className="flex flex-wrap items-center gap-3">
            <h1 className="text-2xl font-semibold">{agent.name}</h1>
            {agent.runtime_kind ? (
              <Badge variant="secondary">{agent.runtime_kind}</Badge>
            ) : null}
          </div>
          <p className="text-muted-foreground mt-1 font-mono text-xs">
            {agent.id}
          </p>
        </div>
        {agent.id ? (
          <Link to="/agents/$agentId/edit" params={{ agentId: agent.id }}>
            <Button variant="outline">Edit</Button>
          </Link>
        ) : null}
      </div>

      {providerWarning ? (
        <Card className="border-destructive/50">
          <CardContent className="flex items-start gap-2 py-4 text-sm">
            <AlertTriangle className="text-destructive mt-0.5 size-4 shrink-0" />
            <span>
              This agent resolves to provider{' '}
              <span className="font-medium">{provider}</span>, which has no API
              key configured on the runtime. Runs will fail until it's set.
            </span>
          </CardContent>
        </Card>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Model</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2 text-sm">
            <Row label="Provider" value={agent.provider || `${provider} (default)`} />
            <Row label="Model" value={agent.model || '(runtime default)'} />
            <Row label="Approval mode" value={agent.approval_mode ?? 'never'} />
            <Row
              label="Default invocation"
              value={agent.default_invocation_mode ?? 'autonomous'}
            />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Access</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3 text-sm">
            <Chips label="Allowed tools" values={agent.allowed_tools} />
            <Chips label="Allowed targets" values={agent.allowed_targets} />
          </CardContent>
        </Card>
      </div>

      {agent.system_prompt ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">System prompt</CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="bg-muted overflow-x-auto rounded-md p-3 text-xs whitespace-pre-wrap">
              {agent.system_prompt}
            </pre>
          </CardContent>
        </Card>
      ) : null}

      {agent.execution_config &&
      Object.keys(agent.execution_config).length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Execution config</CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="bg-muted overflow-x-auto rounded-md p-3 text-xs">
              {JSON.stringify(agent.execution_config, null, 2)}
            </pre>
          </CardContent>
        </Card>
      ) : null}
    </div>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between gap-4">
      <span className="text-muted-foreground">{label}</span>
      <span>{value}</span>
    </div>
  )
}

function Chips({ label, values }: { label: string; values?: Array<string> }) {
  return (
    <div>
      <p className="text-muted-foreground mb-1">{label}</p>
      {values && values.length > 0 ? (
        <div className="flex flex-wrap gap-1">
          {values.map((v) => (
            <Badge key={v} variant="outline" className="font-mono text-xs">
              {v}
            </Badge>
          ))}
        </div>
      ) : (
        <span className="text-muted-foreground">—</span>
      )}
    </div>
  )
}
