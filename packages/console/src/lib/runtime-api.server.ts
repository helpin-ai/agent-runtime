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
  // Extra query params merged on top of the always-present app_id.
  query?: Record<string, string | undefined>
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const url = new URL(`${BASE_URL}${API_PREFIX}${path}`)
  url.searchParams.set('app_id', APP_ID)
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

export function consoleConnection(): ConsoleConnection {
  return {
    base_url: BASE_URL,
    api_prefix: API_PREFIX,
    app_id: APP_ID,
    token_set: TOKEN !== '',
  }
}

// Opens the runtime's SSE stream for a run, injecting the token server-side.
// The raw fetch Response is returned so a server route can pipe `body` straight
// through to the browser's EventSource.
export function openRunEventStream(
  runId: string,
  signal?: AbortSignal,
): Promise<Response> {
  const url = new URL(
    `${BASE_URL}${API_PREFIX}/runs/${encodeURIComponent(runId)}/events`,
  )
  url.searchParams.set('app_id', APP_ID)
  const headers: Record<string, string> = { Accept: 'text/event-stream' }
  if (TOKEN) headers.Authorization = `Bearer ${TOKEN}`
  return fetch(url, { headers, signal })
}

export const runtimeApi = {
  getCapabilities: () => request<Capabilities>('/capabilities'),
  getAppHealth: () => request<AppHealth>('/app-health'),
  listAgents: () => request<Array<Agent>>('/agents'),
  getAgent: (agentId: string) =>
    request<Agent>(`/agents/${encodeURIComponent(agentId)}`),
  createAgent: (agent: Agent) =>
    request<Agent>('/agents', { method: 'POST', body: agent }),
  updateAgent: (agentId: string, agent: Agent) =>
    request<Agent>(`/agents/${encodeURIComponent(agentId)}`, {
      method: 'PUT',
      body: agent,
    }),

  listRuns: () => request<Array<AgentRun>>('/runs'),
  searchRuns: (input: { q?: string; status?: string; limit: number; offset: number }) =>
    request<RunPage>('/runs/search', {
      query: {
        q: input.q,
        status: input.status,
        limit: String(input.limit),
        offset: String(input.offset),
      },
    }),
  getRun: (runId: string) => request<AgentRun>(runPath(runId)),
  startRun: (req: StartRunRequest) =>
    request<AgentRun>('/runs', { method: 'POST', body: req }),

  listMessages: (runId: string) =>
    request<Array<Message>>(runPath(runId, '/messages')),
  listArtifacts: (runId: string) =>
    request<Array<Artifact>>(runPath(runId, '/artifacts')),
  listInteractions: (runId: string) =>
    request<Array<Interaction>>(runPath(runId, '/interactions')),
  listToolCalls: (runId: string) =>
    request<Array<ToolCall>>(runPath(runId, '/tool-calls')),
  listEvents: (runId: string) =>
    request<Array<RunEvent>>(runPath(runId, '/events/history')),
  getRunExecution: (runId: string) =>
    request<RunExecutionInfo>(runPath(runId, '/execution')),

  sendMessage: (runId: string, content: string) =>
    request<Message>(runPath(runId, '/messages'), {
      method: 'POST',
      body: { role: 'user', content },
    }),
  resumeRun: (runId: string, payload: ResumeRunRequest) =>
    request<AgentRun>(runPath(runId, '/resume'), {
      method: 'POST',
      body: payload,
    }),
  approveRun: (runId: string) =>
    request<AgentRun>(runPath(runId, '/approve'), { method: 'POST', body: {} }),
  requestChanges: (runId: string, content: string) =>
    request<AgentRun>(runPath(runId, '/request-changes'), {
      method: 'POST',
      body: { content },
    }),
  cancelRun: (runId: string) =>
    request<AgentRun>(runPath(runId, '/cancel'), { method: 'POST', body: {} }),
}
