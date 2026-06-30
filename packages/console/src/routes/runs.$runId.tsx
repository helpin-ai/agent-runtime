import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '~/components/ui/card'
import { RunStatusBadge } from '~/components/run-status-badge'
import { cn } from '~/lib/utils'
import { useRunStream } from '~/lib/use-run-stream'
import { runDetailQuery } from '~/lib/queries'
import { approveRun, cancelRun, requestChanges } from '~/lib/runtime-fns'
import { isActiveRun } from '~/lib/types'

const TABS = ['transcript', 'tools', 'interactions', 'artifacts'] as const
type Tab = (typeof TABS)[number]

export const Route = createFileRoute('/runs/$runId')({
  validateSearch: (search: Record<string, unknown>): { tab?: Tab } => {
    const tab = search.tab as Tab
    return TABS.includes(tab) ? { tab } : {}
  },
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(runDetailQuery(params.runId)),
  component: RunDetail,
})

function RunDetail() {
  const { runId } = Route.useParams()
  const { tab = 'transcript' } = Route.useSearch()
  const navigate = useNavigate({ from: Route.fullPath })
  const queryClient = useQueryClient()
  const { data } = useSuspenseQuery(runDetailQuery(runId))
  const { run, messages, toolCalls, interactions, artifacts } = data
  const stream = useRunStream(runId)

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ['runs', runId, 'detail'] })

  const approve = useMutation({
    mutationFn: () => approveRun({ data: runId }),
    onSuccess: invalidate,
  })
  const cancel = useMutation({
    mutationFn: () => cancelRun({ data: runId }),
    onSuccess: invalidate,
  })
  const [changes, setChanges] = useState('')
  const reqChanges = useMutation({
    mutationFn: () => requestChanges({ data: { runId, content: changes } }),
    onSuccess: () => {
      setChanges('')
      return invalidate()
    },
  })

  const counts: Record<Tab, number> = {
    transcript: messages.length,
    tools: toolCalls.length,
    interactions: interactions.length,
    artifacts: artifacts.length,
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <div className="flex items-center gap-3">
            <h1 className="font-mono text-xl font-semibold">{run.id}</h1>
            <RunStatusBadge status={run.status} />
            {stream.connected ? (
              <span className="flex items-center gap-1.5 text-xs text-emerald-500">
                <span className="relative flex size-2">
                  <span className="absolute inline-flex size-full animate-ping rounded-full bg-emerald-500 opacity-75" />
                  <span className="relative inline-flex size-2 rounded-full bg-emerald-500" />
                </span>
                live
              </span>
            ) : null}
          </div>
          <p className="text-muted-foreground mt-1 text-sm">
            {run.target.display?.title ?? `${run.target.type}:${run.target.id}`}
            {run.pause_reason ? ` · paused: ${run.pause_reason}` : ''}
          </p>
        </div>
        <div className="flex gap-2">
          <Button
            size="sm"
            onClick={() => approve.mutate()}
            disabled={run.status !== 'paused' || approve.isPending}
          >
            Approve
          </Button>
          <Button
            size="sm"
            variant="destructive"
            onClick={() => cancel.mutate()}
            disabled={!isActiveRun(run.status) || cancel.isPending}
          >
            Cancel
          </Button>
        </div>
      </div>

      {run.error_message ? (
        <Card className="border-destructive/50">
          <CardHeader>
            <CardTitle className="text-destructive text-base">Error</CardTitle>
          </CardHeader>
          <CardContent className="text-sm">{run.error_message}</CardContent>
        </Card>
      ) : null}

      <div className="flex gap-1 border-b">
        {TABS.map((t) => (
          <button
            key={t}
            onClick={() => navigate({ search: { tab: t } })}
            className={cn(
              'border-b-2 px-3 py-2 text-sm font-medium capitalize transition-colors',
              tab === t
                ? 'border-primary text-foreground'
                : 'text-muted-foreground hover:text-foreground border-transparent',
            )}
          >
            {t}
            <span className="text-muted-foreground ml-1.5 text-xs">
              {counts[t]}
            </span>
          </button>
        ))}
      </div>

      {tab === 'transcript' ? (
        <div className="space-y-3">
          {messages.length === 0 ? (
            <Empty label="No messages yet." />
          ) : (
            messages.map((m, i) => (
              <Card key={m.id ?? i}>
                <CardContent className="pt-0">
                  <div className="text-muted-foreground mb-1 text-xs font-medium uppercase">
                    {m.role ?? 'message'}
                    {m.message_type ? ` · ${m.message_type}` : ''}
                  </div>
                  <pre className="whitespace-pre-wrap font-sans text-sm">
                    {m.content}
                  </pre>
                </CardContent>
              </Card>
            ))
          )}
          {stream.streaming && stream.liveText ? (
            <Card className="border-emerald-500/40">
              <CardContent className="pt-0">
                <div className="text-muted-foreground mb-1 flex items-center gap-1.5 text-xs font-medium uppercase">
                  assistant
                  <span className="size-1.5 animate-pulse rounded-full bg-emerald-500" />
                </div>
                <pre className="whitespace-pre-wrap font-sans text-sm">
                  {stream.liveText}
                </pre>
              </CardContent>
            </Card>
          ) : null}
          {run.status === 'paused' ? (
            <Card>
              <CardContent className="space-y-2 pt-0">
                <textarea
                  value={changes}
                  onChange={(e) => setChanges(e.target.value)}
                  placeholder="Request changes or reply…"
                  className="border-input bg-background min-h-20 w-full rounded-md border p-2 text-sm"
                />
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => reqChanges.mutate()}
                  disabled={!changes.trim() || reqChanges.isPending}
                >
                  Send
                </Button>
              </CardContent>
            </Card>
          ) : null}
        </div>
      ) : null}

      {tab === 'tools' ? (
        <div className="space-y-3">
          {toolCalls.length === 0 ? (
            <Empty label="No tool calls." />
          ) : (
            toolCalls.map((tc, i) => (
              <Card key={tc.id ?? i}>
                <CardContent className="space-y-2 pt-0">
                  <div className="flex items-center gap-2">
                    <span className="font-mono text-sm font-medium">
                      {tc.tool_name}
                    </span>
                    {tc.mutating ? (
                      <Badge variant="destructive">mutating</Badge>
                    ) : null}
                    {tc.error ? <Badge variant="destructive">error</Badge> : null}
                  </div>
                  <Json value={tc.input} label="input" />
                  {tc.error ? (
                    <p className="text-destructive text-sm">{tc.error}</p>
                  ) : (
                    <Json value={tc.output} label="output" />
                  )}
                </CardContent>
              </Card>
            ))
          )}
        </div>
      ) : null}

      {tab === 'interactions' ? (
        <div className="space-y-3">
          {interactions.length === 0 ? (
            <Empty label="No interactions." />
          ) : (
            interactions.map((it, i) => (
              <Card key={it.id ?? i}>
                <CardContent className="space-y-1 pt-0">
                  <div className="flex items-center gap-2">
                    <span className="text-sm font-medium">
                      {it.title ?? it.interaction_kind ?? 'interaction'}
                    </span>
                    {it.status ? (
                      <Badge variant="secondary">{it.status}</Badge>
                    ) : null}
                  </div>
                  {it.summary ? (
                    <p className="text-muted-foreground text-sm">{it.summary}</p>
                  ) : null}
                </CardContent>
              </Card>
            ))
          )}
        </div>
      ) : null}

      {tab === 'artifacts' ? (
        <div className="space-y-3">
          {artifacts.length === 0 ? (
            <Empty label="No artifacts." />
          ) : (
            artifacts.map((a, i) => (
              <Card key={a.id ?? i}>
                <CardContent className="space-y-2 pt-0">
                  <div className="flex items-center gap-2">
                    <Badge variant="secondary">{a.artifact_type ?? 'artifact'}</Badge>
                    {a.format ? (
                      <span className="text-muted-foreground text-xs">
                        {a.format}
                      </span>
                    ) : null}
                  </div>
                  {a.inline_content ? (
                    <pre className="bg-muted overflow-x-auto rounded-md p-2 text-xs">
                      {a.inline_content}
                    </pre>
                  ) : null}
                </CardContent>
              </Card>
            ))
          )}
        </div>
      ) : null}
    </div>
  )
}

function Empty({ label }: { label: string }) {
  return <p className="text-muted-foreground py-8 text-center text-sm">{label}</p>
}

function Json({ value, label }: { value: unknown; label: string }) {
  if (value == null) return null
  return (
    <div>
      <div className="text-muted-foreground mb-1 text-xs">{label}</div>
      <pre className="bg-muted overflow-x-auto rounded-md p-2 text-xs">
        {JSON.stringify(value, null, 2)}
      </pre>
    </div>
  )
}
