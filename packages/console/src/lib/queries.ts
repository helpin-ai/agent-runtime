import { queryOptions } from '@tanstack/react-query'
import {
  getAgent,
  getAppCatalog,
  getRunDetail,
  getSystemConfig,
  listAgents,
  listRuns,
  searchRuns,
} from './runtime-fns'

// Live-ish polling cadence for active runs. SSE (see README) supersedes this
// once the runtime exposes a stream endpoint.
export const RUN_POLL_MS = 3000

export const appCatalogQuery = () =>
  queryOptions({
    queryKey: ['app-catalog'],
    queryFn: () => getAppCatalog(),
    staleTime: 60_000,
  })

export const systemConfigQuery = (appId: string) =>
  queryOptions({
    queryKey: ['apps', appId, 'system-config'],
    queryFn: () => getSystemConfig({ data: { appId } }),
    staleTime: 30_000,
  })

export const agentsQuery = (appId: string) =>
  queryOptions({
    queryKey: ['apps', appId, 'agents'],
    queryFn: () => listAgents({ data: { appId } }),
  })

export const agentQuery = (appId: string, agentId: string) =>
  queryOptions({
    queryKey: ['apps', appId, 'agents', agentId],
    queryFn: () => getAgent({ data: { appId, agentId } }),
  })

export const runsQuery = (appId: string) =>
  queryOptions({
    queryKey: ['apps', appId, 'runs'],
    queryFn: () => listRuns({ data: { appId } }),
    refetchInterval: RUN_POLL_MS,
  })

export interface RunSearchInput {
  q?: string
  status?: string
  limit: number
  offset: number
}

export const runSearchQuery = (appId: string, input: RunSearchInput) =>
  queryOptions({
    queryKey: ['apps', appId, 'runs', 'search', input],
    queryFn: () => searchRuns({ data: { appId, ...input } }),
    placeholderData: (previous) => previous,
  })

// SSE (useRunStream) drives real-time detail updates; this interval is only a
// fallback for a dropped stream, so it's slow. Stops once the run is terminal.
export const RUN_DETAIL_FALLBACK_MS = 15_000

export const runDetailQuery = (appId: string, runId: string) =>
  queryOptions({
    queryKey: ['apps', appId, 'runs', runId, 'detail'],
    queryFn: () => getRunDetail({ data: { appId, runId } }),
    refetchInterval: (query) => {
      const status = query.state.data?.run.status
      if (status === 'completed' || status === 'failed' || status === 'cancelled') {
        return false
      }
      return RUN_DETAIL_FALLBACK_MS
    },
  })
