import { createFileRoute } from '@tanstack/react-router'
import { useSuspenseQuery } from '@tanstack/react-query'
import { CheckCircle2, XCircle } from 'lucide-react'
import { Badge } from '~/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '~/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '~/components/ui/table'
import { systemConfigQuery } from '~/lib/queries'

export const Route = createFileRoute('/config')({
  loader: ({ context }) =>
    context.queryClient.ensureQueryData(systemConfigQuery()),
  component: ConfigView,
})

function Yes() {
  return <CheckCircle2 className="size-4 text-emerald-500" />
}
function No() {
  return <XCircle className="text-muted-foreground size-4" />
}

function ConfigView() {
  const { data } = useSuspenseQuery(systemConfigQuery())
  const { capabilities: caps, connection: conn, appHealth } = data
  const currentApp = caps.apps?.find((app) => app.app_id === conn.app_id)

  const toolsByCategory = caps.tools.reduce<Record<string, number>>(
    (acc, t) => {
      const key = t.category || 'uncategorized'
      acc[key] = (acc[key] ?? 0) + 1
      return acc
    },
    {},
  )

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Configuration</h1>
        <p className="text-muted-foreground text-sm">
          What this runtime has wired — providers, storage, durability, tools,
          and skills. Read-only; values come from the runtime's environment.
        </p>
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        {/* Console connection */}
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Console connection</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2 text-sm">
            <Row label="Runtime URL" value={conn.base_url} mono />
            <Row label="API prefix" value={conn.api_prefix} mono />
            <Row label="App ID" value={conn.app_id} mono />
            <div className="flex items-center justify-between">
              <span className="text-muted-foreground">Service token</span>
              {conn.token_set ? <Yes /> : <No />}
            </div>
          </CardContent>
        </Card>

        {/* Runtime summary */}
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Runtime</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2 text-sm">
            <Row label="Store driver" value={caps.store.driver} />
            <div className="flex items-center justify-between">
              <span className="text-muted-foreground">Durable (Temporal)</span>
              {caps.durable.enabled ? <Yes /> : <No />}
            </div>
            {caps.durable.enabled ? (
              <>
                <Row label="Temporal address" value={caps.durable.temporal_address ?? '—'} mono />
                <Row label="Namespace" value={caps.durable.namespace ?? '—'} mono />
              </>
            ) : null}
            <div className="flex items-center justify-between">
              <span className="text-muted-foreground">Service auth</span>
              {caps.service_auth_enabled ? <Yes /> : <No />}
            </div>
            <Row
              label="Runtime kinds"
              value={caps.runtime_kinds.join(', ')}
            />
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Host app integration</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
            <Row label="App" value={conn.app_id} mono />
            <span className="text-muted-foreground">
              {caps.apps?.length ?? 0} configured app{caps.apps?.length === 1 ? '' : 's'}
            </span>
          </div>
          {appHealth.error ? (
            <p className="text-destructive text-sm">{appHealth.error}</p>
          ) : null}
          {appHealth.components.length > 0 ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Component</TableHead>
                  <TableHead>Transport</TableHead>
                  <TableHead>Endpoint</TableHead>
                  <TableHead>Auth</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {appHealth.components.map((component) => (
                  <TableRow key={`${component.kind}:${component.name}`}>
                    <TableCell className="font-medium">{component.name}</TableCell>
                    <TableCell className="text-muted-foreground">{component.transport || '—'}</TableCell>
                    <TableCell className="max-w-md truncate font-mono text-xs">{component.url || 'local'}</TableCell>
                    <TableCell>{component.auth_configured ? <Yes /> : <No />}</TableCell>
                    <TableCell>
                      <span className={component.status === 'error' ? 'text-destructive' : 'text-emerald-500'}>
                        {component.status}
                        {component.http_status ? ` (${component.http_status})` : ''}
                      </span>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ) : currentApp ? (
            <p className="text-muted-foreground text-sm">No host adapters are configured for this app.</p>
          ) : (
            <p className="text-destructive text-sm">This console app ID is not present in AGENT_RUNTIME_APP_CONFIG.</p>
          )}
          <div className="border-t pt-3 text-xs text-muted-foreground">
            Validate before deployment with{' '}
            <code className="text-foreground">agent-runtime-config validate config.yaml</code>
            {' '}and probe adapters with{' '}
            <code className="text-foreground">agent-runtime-config doctor config.yaml {conn.app_id}</code>.
          </div>
        </CardContent>
      </Card>

      {/* Providers */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Model providers</CardTitle>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Provider</TableHead>
                <TableHead>Configured</TableHead>
                <TableHead>Default model</TableHead>
                <TableHead>Base URL override</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {caps.providers.map((p) => (
                <TableRow key={p.name}>
                  <TableCell className="font-medium">{p.name}</TableCell>
                  <TableCell>{p.configured ? <Yes /> : <No />}</TableCell>
                  <TableCell className="text-muted-foreground font-mono text-xs">
                    {p.default_model ?? '—'}
                  </TableCell>
                  <TableCell>{p.base_url_overridden ? <Yes /> : <No />}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* Tools */}
      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            Tools <Badge variant="secondary">{caps.tools.length}</Badge>
          </CardTitle>
          <div className="flex flex-wrap gap-1 pt-1">
            {Object.entries(toolsByCategory).map(([cat, n]) => (
              <Badge key={cat} variant="outline" className="text-muted-foreground">
                {cat}: {n}
              </Badge>
            ))}
          </div>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Category</TableHead>
                <TableHead>Mutating</TableHead>
                <TableHead>Description</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {caps.tools.map((t) => (
                <TableRow key={t.name}>
                  <TableCell className="font-mono text-xs font-medium">
                    {t.name}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {t.category || '—'}
                  </TableCell>
                  <TableCell>
                    {t.mutating ? (
                      <Badge variant="destructive">mutating</Badge>
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  <TableCell className="text-muted-foreground max-w-md truncate">
                    {t.description ?? ''}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      {/* Skills */}
      {caps.skills && caps.skills.length > 0 ? (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">
              Skills <Badge variant="secondary">{caps.skills.length}</Badge>
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="grid gap-2 sm:grid-cols-2">
              {caps.skills.map((s) => (
                <div key={s.key} className="rounded-md border p-2">
                  <p className="text-sm font-medium">{s.title || s.key}</p>
                  {s.description ? (
                    <p className="text-muted-foreground text-xs">
                      {s.description}
                    </p>
                  ) : null}
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      ) : null}
    </div>
  )
}

function Row({
  label,
  value,
  mono,
}: {
  label: string
  value: string
  mono?: boolean
}) {
  return (
    <div className="flex items-center justify-between gap-4">
      <span className="text-muted-foreground">{label}</span>
      <span className={mono ? 'font-mono text-xs' : ''}>{value}</span>
    </div>
  )
}
