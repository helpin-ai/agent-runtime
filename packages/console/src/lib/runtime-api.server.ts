// SERVER-ONLY. Imported exclusively by *.server.ts modules / createServerFn
// handlers. The bearer token lives here and must never reach the browser bundle.
import type {
  Agent,
  AgentRun,
  AppHealth,
  Artifact,
  Capabilities,
  Interaction,
  Message,
  RunEvent,
  RunExecutionInfo,
  RunPage,
  ResumeRunRequest,
  StartRunRequest,
  ToolCall,
} from './types'

const BASE_URL = (
  process.env.AGENT_RUNTIME_BASE_URL ?? 'http://localhost:8090'
).replace(/\/+$/, '')

// Use the versioned API by default. The legacy `/internal` alias is protected
// by the same service-token middleware when AGENT_RUNTIME_SERVICE_TOKEN is set.
const API_PREFIX = process.env.AGENT_RUNTIME_API_PREFIX ?? '/v1'
const TOKEN = process.env.AGENT_RUNTIME_SERVICE_TOKEN ?? ''

// Host application id every Agent Runtime endpoint is scoped to.
export const APP_ID = process.env.AGENT_RUNTIME_APP_ID ?? 'host_app'

interface RequestOptions {
  method?: string
  body?: unknown
  // null intentionally omits app_id for the app catalog endpoint.
  appId?: string | null
  // Extra query params merged on top of the always-present app_id.
  query?: Record<string, string | undefined>
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const url = new URL(`${BASE_URL}${API_PREFIX}${path}`)
  const appId = options.appId === undefined ? APP_ID : options.appId
  if (appId !== null) url.searchParams.set('app_id', appId)
  for (const [key, value] of Object.entries(options.query ?? {})) {
    if (value != null) url.searchParams.set(key, value)
  }

  const headers: Record<string, string> = {}
  if (TOKEN) headers.Authorization = `Bearer ${TOKEN}`
  if (options.body !== undefined) headers['Content-Type'] = 'application/json'

  const res = await fetch(url, {
    method: options.method ?? 'GET',
    headers,
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
  })

  const text = await res.text()
  const data = text ? JSON.parse(text) : null
  if (!res.ok) {
    const message =
      (data && (data.error ?? data.message)) ??
      `Agent Runtime request failed (${res.status})`
    throw new Error(message)
  }
  return data as T
}

const runPath = (runId: string, suffix = '') =>
  `/runs/${encodeURIComponent(runId)}${suffix}`

// Non-secret view of how this console is pointed at the runtime. Reports
// whether a token is set, never the token itself.
export interface ConsoleConnection {
  base_url: string
  api_prefix: string
  app_id: string
  token_set: boolean
}

export function consoleConnection(appId = APP_ID): ConsoleConnection {
  return {
    base_url: BASE_URL,
    api_prefix: API_PREFIX,
    app_id: appId,
    token_set: TOKEN !== '',
  }
}

// Opens the runtime's SSE stream for a run, injecting the token server-side.
// The raw fetch Response is returned so a server route can pipe `body` straight
// through to the browser's EventSource.
export function openRunEventStream(
  appId: string,
  runId: string,
  signal?: AbortSignal,
): Promise<Response> {
  const url = new URL(
    `${BASE_URL}${API_PREFIX}/runs/${encodeURIComponent(runId)}/events`,
  )
  url.searchParams.set('app_id', appId)
  const headers: Record<string, string> = { Accept: 'text/event-stream' }
  if (TOKEN) headers.Authorization = `Bearer ${TOKEN}`
  return fetch(url, { headers, signal })
}

export const runtimeApi = {
  getCapabilities: (appId: string | null) =>
    request<Capabilities>('/capabilities', { appId }),
  getAppHealth: (appId: string) =>
    request<AppHealth>('/app-health', { appId }),
  listAgents: (appId: string) => request<Array<Agent>>('/agents', { appId }),
  getAgent: (appId: string, agentId: string) =>
    request<Agent>(`/agents/${encodeURIComponent(agentId)}`, { appId }),
  createAgent: (appId: string, agent: Agent) =>
    request<Agent>('/agents', { appId, method: 'POST', body: agent }),
  updateAgent: (appId: string, agentId: string, agent: Agent) =>
    request<Agent>(`/agents/${encodeURIComponent(agentId)}`, {
      appId,
      method: 'PUT',
      body: agent,
    }),

  listRuns: (appId: string) => request<Array<AgentRun>>('/runs', { appId }),
  searchRuns: (appId: string, input: { q?: string; status?: string; limit: number; offset: number }) =>
    request<RunPage>('/runs/search', {
      appId,
      query: {
        q: input.q,
        status: input.status,
        limit: String(input.limit),
        offset: String(input.offset),
      },
    }),
  getRun: (appId: string, runId: string) =>
    request<AgentRun>(runPath(runId), { appId }),
  startRun: (appId: string, req: StartRunRequest) =>
    request<AgentRun>('/runs', { appId, method: 'POST', body: req }),

  listMessages: (appId: string, runId: string) =>
    request<Array<Message>>(runPath(runId, '/messages'), { appId }),
  listArtifacts: (appId: string, runId: string) =>
    request<Array<Artifact>>(runPath(runId, '/artifacts'), { appId }),
  listInteractions: (appId: string, runId: string) =>
    request<Array<Interaction>>(runPath(runId, '/interactions'), { appId }),
  listToolCalls: (appId: string, runId: string) =>
    request<Array<ToolCall>>(runPath(runId, '/tool-calls'), { appId }),
  listEvents: (appId: string, runId: string) =>
    request<Array<RunEvent>>(runPath(runId, '/events/history'), { appId }),
  getRunExecution: (appId: string, runId: string) =>
    request<RunExecutionInfo>(runPath(runId, '/execution'), { appId }),

  sendMessage: (appId: string, runId: string, content: string) =>
    request<Message>(runPath(runId, '/messages'), {
      appId,
      method: 'POST',
      body: { role: 'user', content },
    }),
  resumeRun: (appId: string, runId: string, payload: ResumeRunRequest) =>
    request<AgentRun>(runPath(runId, '/resume'), {
      appId,
      method: 'POST',
      body: payload,
    }),
  approveRun: (appId: string, runId: string) =>
    request<AgentRun>(runPath(runId, '/approve'), { appId, method: 'POST', body: {} }),
  requestChanges: (appId: string, runId: string, content: string) =>
    request<AgentRun>(runPath(runId, '/request-changes'), {
      appId,
      method: 'POST',
      body: { content },
    }),
  cancelRun: (appId: string, runId: string) =>
    request<AgentRun>(runPath(runId, '/cancel'), { appId, method: 'POST', body: {} }),
}
