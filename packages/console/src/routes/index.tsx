import type { ReactNode } from 'react'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useSuspenseQuery } from '@tanstack/react-query'
import { AlertTriangle, CheckCircle2 } from 'lucide-react'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '~/components/ui/card'
import { Badge } from '~/components/ui/badge'
import { RunStatusBadge } from '~/components/run-status-badge'
import { agentsQuery, runsQuery, systemConfigQuery } from '~/lib/queries'
import { deriveAttention } from '~/lib/health'
import { ACTIVE_RUN_STATUSES } from '~/lib/types'
import { useAppScope } from '~/lib/app-scope'

export const Route = createFileRoute('/')({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(agentsQuery(context.appId)),
      context.queryClient.ensureQueryData(runsQuery(context.appId)),
      context.queryClient.ensureQueryData(systemConfigQuery(context.appId)),
    ])
  },
  component: Dashboard,
})

function Dashboard() {
  const { appId } = useAppScope()
  const { data: agents } = useSuspenseQuery(agentsQuery(appId))
  const { data: runs } = useSuspenseQuery(runsQuery(appId))
  const { data: config } = useSuspenseQuery(systemConfigQuery(appId))
  const { capabilities } = config

  const active = runs.filter((r) => ACTIVE_RUN_STATUSES.includes(r.status))
  const paused = runs.filter((r) => r.status === 'paused')
  const failed = runs.filter((r) => r.status === 'failed')
  const attention = deriveAttention(runs, agents, capabilities)
  const recent = runs.slice(0, 6)

  const stats = [
    { label: 'Agents', value: agents.length, to: '/agents' as const },
    { label: 'Total runs', value: runs.length, to: '/runs' as const },
    { label: 'Active', value: active.length, to: '/runs' as const },
    { label: 'Failed', value: failed.length, to: '/runs' as const },
  ]

  const configuredProviders = capabilities.providers.filter((p) => p.configured)

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Dashboard</h1>
        <p className="text-muted-foreground text-sm">
          Run activity, configuration, and anything needing attention.
        </p>
      </div>

      {/* Things needing attention */}
      {attention.length > 0 ? (
        <Card className="border-amber-500/40">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <AlertTriangle className="size-4 text-amber-500" />
              Needs attention
              <Badge variant="secondary">{attention.length}</Badge>
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            {attention.slice(0, 6).map((item) => {
              const body = (
                <div className="flex items-start gap-2">
                  <span
                    className={
                      item.severity === 'error'
                        ? 'mt-1.5 size-2 shrink-0 rounded-full bg-red-500'
                        : 'mt-1.5 size-2 shrink-0 rounded-full bg-amber-500'
                    }
                  />
                  <div className="min-w-0">
                    <p className="text-sm font-medium">{item.title}</p>
                    {item.detail ? (
                      <p className="text-muted-foreground truncate text-xs">
                        {item.detail}
                      </p>
                    ) : null}
                  </div>
                </div>
              )
              return item.runId ? (
                <Link
                  key={item.id}
                  to="/runs/$runId"
                  params={{ runId: item.runId }}
                  className="hover:bg-accent block rounded-md p-1.5"
                >
                  {body}
                </Link>
              ) : (
                <div key={item.id} className="p-1.5">
                  {body}
                </div>
              )
            })}
          </CardContent>
        </Card>
      ) : (
        <Card className="border-emerald-500/30">
          <CardContent className="flex items-center gap-2 py-4 text-sm">
            <CheckCircle2 className="size-4 text-emerald-500" />
            Nothing needs attention.
          </CardContent>
        </Card>
      )}

      {/* Run activity */}
      <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
        {stats.map((s) => (
          <Link key={s.label} to={s.to}>
            <Card className="hover:border-ring transition-colors">
              <CardHeader>
                <CardDescription>{s.label}</CardDescription>
                <CardTitle className="text-3xl">{s.value}</CardTitle>
              </CardHeader>
            </Card>
          </Link>
        ))}
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        {/* Configuration summary */}
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Configuration</CardTitle>
            <CardDescription>
              <Link to="/config" className="underline">
                View full configuration →
              </Link>
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3 text-sm">
            <SummaryRow label="Providers">
              <div className="flex flex-wrap gap-1">
                {capabilities.providers.map((p) => (
                  <Badge
                    key={p.name}
                    variant={p.configured ? 'default' : 'outline'}
                    className={p.configured ? '' : 'text-muted-foreground'}
                  >
                    {p.name}
                  </Badge>
                ))}
              </div>
            </SummaryRow>
            <SummaryRow label="Store">
              <span className="text-muted-foreground">
                {capabilities.store.driver}
              </span>
            </SummaryRow>
            <SummaryRow label="Durable (Temporal)">
              <Badge variant={capabilities.durable.enabled ? 'default' : 'outline'}>
                {capabilities.durable.enabled ? 'enabled' : 'disabled'}
              </Badge>
            </SummaryRow>
            <SummaryRow label="Tools / Skills">
              <span className="text-muted-foreground">
                {capabilities.tools.length} tools · {capabilities.skills?.length ?? 0} skills
              </span>
            </SummaryRow>
            {configuredProviders.length === 0 ? (
              <p className="text-destructive text-xs">
                No model provider is configured — native_sdk agents cannot run.
              </p>
            ) : null}
          </CardContent>
        </Card>

        {/* Recent runs */}
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Recent runs</CardTitle>
            <CardDescription>
              {paused.length} paused · {active.length} active
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-1">
            {recent.length === 0 ? (
              <p className="text-muted-foreground text-sm">No runs yet.</p>
            ) : (
              recent.map((run) => (
                <Link
                  key={run.id}
                  to="/runs/$runId"
                  params={{ runId: run.id }}
                  className="hover:bg-accent flex items-center justify-between gap-2 rounded-md px-2 py-1.5"
                >
                  <span className="truncate font-mono text-xs">{run.id}</span>
                  <RunStatusBadge status={run.status} />
                </Link>
              ))
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  )
}

function SummaryRow({
  label,
  children,
}: {
  label: string
  children: ReactNode
}) {
  return (
    <div className="flex items-center justify-between gap-4">
      <span className="text-muted-foreground">{label}</span>
      {children}
    </div>
  )
}
