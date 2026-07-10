// Server functions = the BFF boundary. Each runs only on the server, where the
// service token is injected, and is callable from loaders / TanStack Query on
// the client without ever exposing the token or the runtime's base URL.
import { createServerFn } from '@tanstack/react-start'
import { consoleConnection, runtimeApi } from './runtime-api.server'
import type { Agent, ResumeRunRequest, StartRunRequest } from './types'

const appInput = (input: { appId: string }) => {
  const appId = input.appId.trim()
  if (!appId) throw new Error('app_id is required')
  return { appId }
}

export const getAppCatalog = createServerFn({ method: 'GET' }).handler(async () => {
  const connection = consoleConnection()
  const capabilities = await runtimeApi.getCapabilities(null)
  return {
    apps: capabilities.apps ?? [],
    default_app_id: connection.app_id,
  }
})

export const getCapabilities = createServerFn({ method: 'GET' })
  .validator(appInput)
  .handler(({ data }) => runtimeApi.getCapabilities(data.appId))

// Combines what the runtime reports with how this console is wired to it.
export const getSystemConfig = createServerFn({ method: 'GET' })
  .validator(appInput)
  .handler(async ({ data }) => {
    const connection = consoleConnection(data.appId)
    const [capabilities, appHealth] = await Promise.all([
      runtimeApi.getCapabilities(data.appId),
      runtimeApi.getAppHealth(data.appId).catch((error: Error) => ({
        app_id: connection.app_id,
        components: [],
        error: error.message,
      })),
    ])
    return { capabilities, appHealth, connection }
  })

export const listAgents = createServerFn({ method: 'GET' })
  .validator(appInput)
  .handler(({ data }) => runtimeApi.listAgents(data.appId))

export const getAgent = createServerFn({ method: 'GET' })
  .validator((input: { appId: string; agentId: string }) => input)
  .handler(({ data }) => runtimeApi.getAgent(data.appId, data.agentId))

export const createAgent = createServerFn({ method: 'POST' })
  .validator((input: { appId: string; agent: Agent }) => input)
  .handler(({ data }) => runtimeApi.createAgent(data.appId, { ...data.agent, app_id: data.appId }))

export const updateAgent = createServerFn({ method: 'POST' })
  .validator((input: { appId: string; agentId: string; agent: Agent }) => input)
  .handler(({ data }) => runtimeApi.updateAgent(data.appId, data.agentId, { ...data.agent, app_id: data.appId }))

export const listRuns = createServerFn({ method: 'GET' })
  .validator(appInput)
  .handler(({ data }) => runtimeApi.listRuns(data.appId))

export const searchRuns = createServerFn({ method: 'GET' })
  .validator((input: { appId: string; q?: string; status?: string; limit: number; offset: number }) => input)
  .handler(({ data }) => runtimeApi.searchRuns(data.appId, data))

export const getRun = createServerFn({ method: 'GET' })
  .validator((input: { appId: string; runId: string }) => input)
  .handler(({ data }) => runtimeApi.getRun(data.appId, data.runId))

// Aggregate every per-run resource in one round trip so the run-detail loader
// and its polling refetch stay a single request.
export const getRunDetail = createServerFn({ method: 'GET' })
  .validator((input: { appId: string; runId: string }) => input)
  .handler(async ({ data: { appId, runId } }) => {
    const [run, messages, artifacts, interactions, toolCalls, events, execution] =
      await Promise.all([
        runtimeApi.getRun(appId, runId),
        runtimeApi.listMessages(appId, runId),
        runtimeApi.listArtifacts(appId, runId),
        runtimeApi.listInteractions(appId, runId),
        runtimeApi.listToolCalls(appId, runId),
        runtimeApi.listEvents(appId, runId),
        runtimeApi.getRunExecution(appId, runId).catch((error: Error) => ({
          execution_mode: 'durable',
          state: 'unavailable',
          error: error.message,
        })),
      ])
    return { run, messages, artifacts, interactions, toolCalls, events, execution }
  })

export const startRun = createServerFn({ method: 'POST' })
  .validator((input: { appId: string; request: StartRunRequest }) => input)
  .handler(({ data }) => runtimeApi.startRun(data.appId, { ...data.request, app_id: data.appId }))

export const sendMessage = createServerFn({ method: 'POST' })
  .validator((input: { appId: string; runId: string; content: string }) => input)
  .handler(({ data }) => runtimeApi.sendMessage(data.appId, data.runId, data.content))

export const resumeRun = createServerFn({ method: 'POST' })
  .validator((input: { appId: string; runId: string; payload: ResumeRunRequest }) => input)
  .handler(({ data }) => runtimeApi.resumeRun(data.appId, data.runId, data.payload))

export const approveRun = createServerFn({ method: 'POST' })
  .validator((input: { appId: string; runId: string }) => input)
  .handler(({ data }) => runtimeApi.approveRun(data.appId, data.runId))

export const requestChanges = createServerFn({ method: 'POST' })
  .validator((input: { appId: string; runId: string; content: string }) => input)
  .handler(({ data }) => runtimeApi.requestChanges(data.appId, data.runId, data.content))

export const cancelRun = createServerFn({ method: 'POST' })
  .validator((input: { appId: string; runId: string }) => input)
  .handler(({ data }) => runtimeApi.cancelRun(data.appId, data.runId))
