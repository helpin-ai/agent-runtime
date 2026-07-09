import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'

export interface RunStreamState {
  // Live assistant text accumulated from *_delta events while a message streams.
  liveText: string
  streaming: boolean
  connected: boolean
}

// Events that mean persisted state changed → refetch the run detail. Deltas are
// handled live and intentionally do NOT trigger a refetch (too chatty).
const REFETCH_EVENTS = new Set([
  'assistant_message_completed',
  'tool_call_finished',
  'tool_call_result',
  'plan_updated',
  'usage.checkpoint',
  'workspace.prepared',
  'workspace.cleaned',
  'run.started',
  'run.paused',
  'run.resumed',
  'run.completed',
  'run.failed',
  'run.cancelled',
])

const TERMINAL_EVENTS = new Set(['run.completed', 'run.failed', 'run.cancelled'])

// Subscribes to a run's SSE stream and keeps the run-detail query fresh,
// surfacing in-flight assistant text. Falls back gracefully: if the stream
// drops, the query's own refetchInterval still covers it.
export function useRunStream(appId: string, runId: string): RunStreamState {
  const queryClient = useQueryClient()
  const [state, setState] = useState<RunStreamState>({
    liveText: '',
    streaming: false,
    connected: false,
  })
  // Debounce refetches so a burst of tool events coalesces into one.
  const refetchTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    const query = new URLSearchParams({ app_id: appId })
    const source = new EventSource(`/api/runs/${encodeURIComponent(runId)}/events?${query}`)

    const invalidateSoon = () => {
      if (refetchTimer.current) return
      refetchTimer.current = setTimeout(() => {
        refetchTimer.current = null
        queryClient.invalidateQueries({ queryKey: ['apps', appId, 'runs', runId, 'detail'] })
        queryClient.invalidateQueries({ queryKey: ['apps', appId, 'runs'] })
      }, 250)
    }

    source.onopen = () =>
      setState((s) => ({ ...s, connected: true }))
    source.onerror = () =>
      setState((s) => ({ ...s, connected: false }))

    source.onmessage = (e) => handle('message', e.data)

    function handle(type: string, raw: string) {
      let evt: { type?: string; data?: Record<string, unknown> }
      try {
        evt = JSON.parse(raw)
      } catch {
        return
      }
      const kind = evt.type ?? type
      const text =
        (evt.data?.text as string | undefined) ??
        (evt.data?.content as string | undefined) ??
        ''

      if (kind === 'assistant_message_started') {
        setState((s) => ({ ...s, liveText: '', streaming: true }))
      } else if (kind === 'assistant_message_delta') {
        setState((s) => ({ ...s, streaming: true, liveText: s.liveText + text }))
      } else if (kind === 'assistant_message_completed') {
        setState((s) => ({ ...s, streaming: false, liveText: '' }))
        invalidateSoon()
      } else if (REFETCH_EVENTS.has(kind)) {
        invalidateSoon()
      }

      if (TERMINAL_EVENTS.has(kind)) {
        setState((s) => ({ ...s, streaming: false, liveText: '' }))
        source.close()
        setState((s) => ({ ...s, connected: false }))
      }
    }

    // Named SSE events (event: <type>) arrive via addEventListener, not onmessage.
    const named = [
      'assistant_message_started',
      'assistant_message_delta',
      'assistant_message_completed',
      'tool_call_started',
      'tool_call_args_delta',
      'tool_call_finished',
      'tool_call_result',
      'plan_updated',
      'message_id',
      'run.queued',
      'run.started',
      'run.paused',
      'run.resumed',
      'run.completed',
      'run.failed',
      'run.cancelled',
      'workspace.prepared',
      'workspace.cleaned',
      'usage.checkpoint',
      'reasoning_message_started',
      'reasoning_message_delta',
      'reasoning_message_completed',
      'activity_snapshot',
      'activity_delta',
    ]
    const listeners = named.map((name) => {
      const fn = (e: MessageEvent) => handle(name, e.data)
      source.addEventListener(name, fn as EventListener)
      return [name, fn] as const
    })

    return () => {
      if (refetchTimer.current) clearTimeout(refetchTimer.current)
      listeners.forEach(([name, fn]) =>
        source.removeEventListener(name, fn as EventListener),
      )
      source.close()
    }
  }, [appId, runId, queryClient])

  return state
}
