import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { useState } from 'react'
import type { ReactNode } from 'react'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '~/components/ui/card'
import { RunStatusBadge } from '~/components/run-status-badge'
import { cn } from '~/lib/utils'
import { useRunStream } from '~/lib/use-run-stream'
import { runDetailQuery } from '~/lib/queries'
import { approveRun, cancelRun, resumeRun } from '~/lib/runtime-fns'
import { isActiveRun } from '~/lib/types'
import type { AgentRun, Artifact, Interaction, JsonValue, Message, RunEvent, RunExecutionInfo, ToolCall } from '~/lib/types'
import { useAppScope } from '~/lib/app-scope'

const TABS = ['overview', 'timeline', 'transcript', 'tools', 'interactions', 'artifacts'] as const
type Tab = (typeof TABS)[number]

export const Route = createFileRoute('/runs/$runId')({
  validateSearch: (search: Record<string, unknown>): { tab?: Tab } => {
    const tab = search.tab as Tab
    return TABS.includes(tab) ? { tab } : {}
  },
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(runDetailQuery(context.appId, params.runId)),
  component: RunDetail,
})

function RunDetail() {
  const { runId } = Route.useParams()
  const { appId } = useAppScope()
  const { tab = 'overview' } = Route.useSearch()
  const navigate = useNavigate({ from: Route.fullPath })
  const queryClient = useQueryClient()
  const { data } = useSuspenseQuery(runDetailQuery(appId, runId))
  const { run, messages, toolCalls, interactions, artifacts, events, execution } = data
  const stream = useRunStream(appId, runId)
  const [reply, setReply] = useState('')

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ['apps', appId, 'runs', runId, 'detail'] })

  const approve = useMutation({
    mutationFn: () => approveRun({ data: { appId, runId } }),
    onSuccess: invalidate,
  })
  const cancel = useMutation({
    mutationFn: () => cancelRun({ data: { appId, runId } }),
    onSuccess: invalidate,
  })
  const respond = useMutation({
    mutationFn: () =>
      resumeRun({
        data: {
          appId,
          runId,
          payload: {
            intent: run.pause_reason === 'human_approval' ? 'request_changes' : 'reply',
            content: reply,
          },
        },
      }),
    onSuccess: () => {
      setReply('')
      return invalidate()
    },
  })

  const counts: Record<Tab, number | null> = {
    overview: null,
    timeline: events.length,
    transcript: messages.length,
    tools: toolCalls.length,
    interactions: interactions.length,
    artifacts: artifacts.length,
  }
  const canApprove = run.status === 'paused' && run.pause_reason === 'human_approval'

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <div className="flex items-center gap-3">
            <h1 className="font-mono text-xl font-semibold">{run.id}</h1>
            <RunStatusBadge status={run.status} />
            {stream.connected ? <span className="text-xs text-emerald-500">live</span> : null}
          </div>
          <p className="text-muted-foreground mt-1 text-sm">
            {run.target.display?.title ?? `${run.target.type}:${run.target.id}`}
            {run.pause_reason && run.pause_reason !== 'none' ? ` · ${run.pause_reason}` : ''}
          </p>
        </div>
        <div className="flex gap-2">
          <Button size="sm" onClick={() => approve.mutate()} disabled={!canApprove || approve.isPending}>
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
        <div className="border-destructive/50 text-destructive border px-3 py-2 text-sm">
          {run.error_message}
        </div>
      ) : null}

      <div className="flex gap-1 overflow-x-auto border-b">
        {TABS.map((item) => (
          <button
            key={item}
            onClick={() => navigate({ search: { tab: item } })}
            className={cn(
              'border-b-2 px-3 py-2 text-sm font-medium capitalize transition-colors',
              tab === item
                ? 'border-primary text-foreground'
                : 'text-muted-foreground hover:text-foreground border-transparent',
            )}
          >
            {item}
            {counts[item] !== null ? (
              <span className="text-muted-foreground ml-1.5 text-xs">{counts[item]}</span>
            ) : null}
          </button>
        ))}
      </div>

      {tab === 'overview' ? <RunOverview run={run} execution={execution} /> : null}
      {tab === 'timeline' ? <Timeline events={events} /> : null}
      {tab === 'transcript' ? (
        <Transcript messages={messages} liveText={stream.liveText} streaming={stream.streaming} />
      ) : null}
      {tab === 'tools' ? <ToolCalls toolCalls={toolCalls} /> : null}
      {tab === 'interactions' ? <Interactions interactions={interactions} /> : null}
      {tab === 'artifacts' ? <Artifacts artifacts={artifacts} /> : null}

      {run.status === 'paused' ? (
        <div className="space-y-2 border-t pt-4">
          <textarea
            value={reply}
            onChange={(event) => setReply(event.target.value)}
            placeholder={canApprove ? 'Describe the required changes…' : 'Reply to the agent…'}
            className="border-input bg-background min-h-20 w-full rounded-md border p-2 text-sm"
          />
          <Button
            size="sm"
            variant="secondary"
            onClick={() => respond.mutate()}
            disabled={!reply.trim() || respond.isPending}
          >
            Send
          </Button>
        </div>
      ) : null}
    </div>
  )
}

function RunOverview({ run, execution }: { run: AgentRun; execution: RunExecutionInfo }) {
  const usage = readUsage(run.output_summary)
  return (
    <div className="grid gap-6 xl:grid-cols-2">
      <Section title="Run">
        <DefinitionList
          rows={[
            ['Agent', <Link to="/agents/$agentId" params={{ agentId: run.agent_id }} className="font-mono hover:underline">{run.agent_id}</Link>],
            ['Host run', run.host_run_id],
            ['Runtime', run.runtime_kind],
            ['Execution mode', run.execution_mode],
            ['Invocation mode', run.invocation_mode],
            ['Created', formatTime(run.created_at)],
            ['Started', formatTime(run.started_at)],
            ['Completed', formatTime(run.completed_at)],
            ['Duration', formatDuration(run.started_at, run.completed_at)],
          ]}
        />
      </Section>
      <Section title="Temporal execution">
        <DefinitionList
          rows={[
            ['State', execution.state],
            ['Inspection error', execution.error],
            ['Workflow ID', execution.workflow_id],
            ['Temporal run', execution.temporal_run_id],
            ['Task queue', execution.task_queue],
            ['History events', numberValue(execution.history_length)],
            ['History size', formatBytes(numberValue(execution.history_size_bytes))],
            ['State transitions', numberValue(execution.state_transition_count)],
          ]}
        />
      </Section>
      <Section title="Usage">
        <DefinitionList
          rows={[
            ['Total tokens', usage.total_tokens],
            ['Input', usage.input_tokens],
            ['Cached input', usage.cached_input_tokens],
            ['Output', usage.output_tokens],
            ['Reasoning output', usage.reasoning_output_tokens],
          ]}
        />
      </Section>
      <Section title="Target and workspace">
        <DefinitionList
          rows={[
            ['Target', `${run.target.type}:${run.target.id}`],
            ['Workspace lease', run.workspace_lease?.id],
            ['Workspace provider', run.workspace_lease?.provider],
            ['Root path', run.workspace_lease?.root_path],
            ['Cleanup', run.workspace_lease?.cleanup_policy],
            ['External actor', run.external_actor_id],
          ]}
        />
      </Section>
      <JsonPanel label="Run input" value={run.input} />
      <JsonPanel label="Output summary" value={run.output_summary} />
    </div>
  )
}

function Timeline({ events }: { events: Array<RunEvent> }) {
  if (events.length === 0) return <Empty label="No persisted events." />
  return (
    <ol className="divide-y border-y">
      {events.map((event) => (
        <li key={event.event_id} className="run-list-item grid gap-1 py-3 sm:grid-cols-[9rem_15rem_1fr] sm:gap-4">
          <time className="text-muted-foreground font-mono text-xs">{formatTime(event.sent_at)}</time>
          <span className="font-mono text-xs">{event.type}</span>
          <div className="min-w-0 text-sm">
            <span>{eventSummary(event)}</span>
            {event.data && Object.keys(event.data).length > 0 ? <JsonPanel label="Event data" value={event.data} compact /> : null}
          </div>
        </li>
      ))}
    </ol>
  )
}

function Transcript({ messages, liveText, streaming }: { messages: Array<Message>; liveText: string; streaming: boolean }) {
  return (
    <div className="space-y-3">
      {messages.length === 0 ? <Empty label="No messages yet." /> : messages.map((message, index) => (
        <Card key={String(message.id ?? index)} className="run-list-item">
          <CardContent className="space-y-2 pt-0">
            <div className="text-muted-foreground flex justify-between text-xs">
              <span>{String(message.role ?? 'message')}{message.message_type ? ` · ${String(message.message_type)}` : ''}</span>
              <span>{formatTime(stringValue(message.created_at))}</span>
            </div>
            <pre className="whitespace-pre-wrap font-sans text-sm">{String(message.content ?? '')}</pre>
            <JsonPanel label="Content blocks" value={message.content_blocks} compact />
            <JsonPanel label="Tool invocations" value={message.tool_invocations} compact />
          </CardContent>
        </Card>
      ))}
      {streaming && liveText ? (
        <div className="border border-emerald-500/40 p-3">
          <div className="text-muted-foreground mb-1 text-xs">assistant · streaming</div>
          <pre className="whitespace-pre-wrap font-sans text-sm">{liveText}</pre>
        </div>
      ) : null}
    </div>
  )
}

function ToolCalls({ toolCalls }: { toolCalls: Array<ToolCall> }) {
  if (toolCalls.length === 0) return <Empty label="No tool calls." />
  return <div className="divide-y border-y">{toolCalls.map((call, index) => (
    <div key={String(call.id ?? index)} className="run-list-item space-y-2 py-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm font-medium">{String(call.tool_name ?? 'tool')}</span>
        {call.mutating ? <Badge variant="destructive">mutating</Badge> : null}
        {call.approval_required ? <Badge variant="outline">approval required</Badge> : null}
        {call.error ? <Badge variant="destructive">error</Badge> : null}
        <span className="text-muted-foreground ml-auto text-xs">{formatTime(stringValue(call.created_at))}</span>
      </div>
      <JsonPanel label="Input" value={call.input} compact />
      {call.error ? <p className="text-destructive text-sm">{String(call.error)}</p> : <JsonPanel label="Output" value={call.output} compact />}
    </div>
  ))}</div>
}

function Interactions({ interactions }: { interactions: Array<Interaction> }) {
  if (interactions.length === 0) return <Empty label="No interactions." />
  return <div className="divide-y border-y">{interactions.map((interaction, index) => (
    <div key={String(interaction.id ?? index)} className="run-list-item space-y-2 py-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium">{String(interaction.title ?? interaction.interaction_kind ?? 'interaction')}</span>
        {interaction.status ? <Badge variant="secondary">{String(interaction.status)}</Badge> : null}
        <span className="text-muted-foreground ml-auto text-xs">{formatTime(stringValue(interaction.created_at))}</span>
      </div>
      {interaction.summary ? <p className="text-muted-foreground text-sm">{String(interaction.summary)}</p> : null}
      {interaction.resolved_by_external_id || interaction.resolved_at ? (
        <p className="text-muted-foreground text-xs">Resolved by {String(interaction.resolved_by_external_id ?? 'unknown')} · {formatTime(stringValue(interaction.resolved_at))}</p>
      ) : null}
      <JsonPanel label="Request" value={interaction.request_payload} compact />
      <JsonPanel label="Response" value={interaction.response_payload} compact />
    </div>
  ))}</div>
}

function Artifacts({ artifacts }: { artifacts: Array<Artifact> }) {
  if (artifacts.length === 0) return <Empty label="No artifacts." />
  return <div className="divide-y border-y">{artifacts.map((artifact, index) => (
    <div key={String(artifact.id ?? index)} className="run-list-item space-y-2 py-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm font-medium">{String(artifact.artifact_type ?? 'artifact')}</span>
        <span className="text-muted-foreground text-xs">{String(artifact.format ?? '')}</span>
        <span className="text-muted-foreground ml-auto text-xs">{formatTime(stringValue(artifact.created_at))}</span>
      </div>
      {artifact.inline_content ? <pre className="bg-muted max-h-96 overflow-auto rounded-md p-2 text-xs">{String(artifact.inline_content)}</pre> : null}
      <JsonPanel label="Metadata" value={artifact.metadata} compact />
    </div>
  ))}</div>
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return <section className="space-y-3 border-t pt-3"><h2 className="text-sm font-semibold">{title}</h2>{children}</section>
}

function DefinitionList({ rows }: { rows: Array<[string, ReactNode]> }) {
  return <dl className="grid grid-cols-[9rem_minmax(0,1fr)] gap-x-4 gap-y-2 text-sm">{rows.map(([label, value]) => (
    <div key={label} className="contents"><dt className="text-muted-foreground">{label}</dt><dd className="min-w-0 break-words font-mono text-xs">{value ?? '—'}</dd></div>
  ))}</dl>
}

function JsonPanel({ value, label, compact = false }: { value: unknown; label: string; compact?: boolean }) {
  if (value == null || value === '' || (typeof value === 'object' && Object.keys(value).length === 0)) return null
  return <details className={compact ? 'text-xs' : 'border-t pt-3 text-xs'}><summary className="text-muted-foreground cursor-pointer select-none">{label}</summary><pre className="bg-muted mt-2 max-h-96 overflow-auto rounded-md p-2">{JSON.stringify(value, null, 2)}</pre></details>
}

function Empty({ label }: { label: string }) {
  return <p className="text-muted-foreground py-8 text-center text-sm">{label}</p>
}

function readUsage(summary?: Record<string, JsonValue>) {
  const usage = ((summary?.usage && typeof summary.usage === 'object' && !Array.isArray(summary.usage) ? summary.usage : summary) ?? {}) as Record<string, JsonValue>
  return {
    total_tokens: numberValue(usage.total_tokens), input_tokens: numberValue(usage.input_tokens),
    cached_input_tokens: numberValue(usage.cached_input_tokens), output_tokens: numberValue(usage.output_tokens),
    reasoning_output_tokens: numberValue(usage.reasoning_output_tokens),
  }
}

function eventSummary(event: RunEvent): string {
  const data = event.data ?? {}
  return stringValue(data.error) || stringValue(data.pause_reason) || stringValue(data.tool_name) || stringValue(data.text) || '—'
}

function stringValue(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

function numberValue(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

function formatTime(value?: string): string {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function formatDuration(start?: string, end?: string): string {
  if (!start) return '—'
  const duration = new Date(end ?? Date.now()).getTime() - new Date(start).getTime()
  if (!Number.isFinite(duration) || duration < 0) return '—'
  if (duration < 60_000) return `${Math.round(duration / 1000)}s`
  if (duration < 3_600_000) return `${Math.round(duration / 60_000)}m`
  return `${(duration / 3_600_000).toFixed(1)}h`
}

function formatBytes(value: number | null): string {
  if (value == null) return '—'
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`
  return `${(value / (1024 * 1024)).toFixed(1)} MB`
}
