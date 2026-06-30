import { queryOptions } from '@tanstack/react-query'
import {
  getAgent,
  getRunDetail,
  getSystemConfig,
  listAgents,
  listRuns,
} from './runtime-fns'

// Live-ish polling cadence for active runs. SSE (see README) supersedes this
// once the runtime exposes a stream endpoint.
export const RUN_POLL_MS = 3000

export const systemConfigQuery = () =>
  queryOptions({
    queryKey: ['system-config'],
    queryFn: () => getSystemConfig(),
    staleTime: 30_000,
  })

export const agentsQuery = () =>
  queryOptions({
    queryKey: ['agents'],
    queryFn: () => listAgents(),
  })

export const agentQuery = (agentId: string) =>
  queryOptions({
    queryKey: ['agents', agentId],
    queryFn: () => getAgent({ data: agentId }),
  })

export const runsQuery = () =>
  queryOptions({
    queryKey: ['runs'],
    queryFn: () => listRuns(),
    refetchInterval: RUN_POLL_MS,
  })

// SSE (useRunStream) drives real-time detail updates; this interval is only a
// fallback for a dropped stream, so it's slow. Stops once the run is terminal.
export const RUN_DETAIL_FALLBACK_MS = 15_000

export const runDetailQuery = (runId: string) =>
  queryOptions({
    queryKey: ['runs', runId, 'detail'],
    queryFn: () => getRunDetail({ data: runId }),
    refetchInterval: (query) => {
      const status = query.state.data?.run.status
      if (status === 'completed' || status === 'failed' || status === 'cancelled') {
        return false
      }
      return RUN_DETAIL_FALLBACK_MS
    },
  })
