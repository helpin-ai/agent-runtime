// Server functions = the BFF boundary. Each runs only on the server, where the
// service token is injected, and is callable from loaders / TanStack Query on
// the client without ever exposing the token or the runtime's base URL.
import { createServerFn } from '@tanstack/react-start'
import { consoleConnection, runtimeApi } from './runtime-api.server'
import type { Agent, ResumeRunRequest, StartRunRequest } from './types'

export const getCapabilities = createServerFn({ method: 'GET' }).handler(() =>
  runtimeApi.getCapabilities(),
)

// Combines what the runtime reports with how this console is wired to it.
export const getSystemConfig = createServerFn({ method: 'GET' }).handler(
  async () => {
    const capabilities = await runtimeApi.getCapabilities()
    return { capabilities, connection: consoleConnection() }
  },
)

export const listAgents = createServerFn({ method: 'GET' }).handler(() =>
  runtimeApi.listAgents(),
)

export const getAgent = createServerFn({ method: 'GET' })
  .validator((agentId: string) => agentId)
  .handler(({ data: agentId }) => runtimeApi.getAgent(agentId))

export const createAgent = createServerFn({ method: 'POST' })
  .validator((agent: Agent) => agent)
  .handler(({ data }) => runtimeApi.createAgent(data))

export const updateAgent = createServerFn({ method: 'POST' })
  .validator((input: { agentId: string; agent: Agent }) => input)
  .handler(({ data }) => runtimeApi.updateAgent(data.agentId, data.agent))

export const listRuns = createServerFn({ method: 'GET' }).handler(() =>
  runtimeApi.listRuns(),
)

export const getRun = createServerFn({ method: 'GET' })
  .validator((runId: string) => runId)
  .handler(({ data: runId }) => runtimeApi.getRun(runId))

// Aggregate every per-run resource in one round trip so the run-detail loader
// and its polling refetch stay a single request.
export const getRunDetail = createServerFn({ method: 'GET' })
  .validator((runId: string) => runId)
  .handler(async ({ data: runId }) => {
    const [run, messages, artifacts, interactions, toolCalls] =
      await Promise.all([
        runtimeApi.getRun(runId),
        runtimeApi.listMessages(runId),
        runtimeApi.listArtifacts(runId),
        runtimeApi.listInteractions(runId),
        runtimeApi.listToolCalls(runId),
      ])
    return { run, messages, artifacts, interactions, toolCalls }
  })

export const startRun = createServerFn({ method: 'POST' })
  .validator((req: StartRunRequest) => req)
  .handler(({ data }) => runtimeApi.startRun(data))

export const sendMessage = createServerFn({ method: 'POST' })
  .validator((input: { runId: string; content: string }) => input)
  .handler(({ data }) => runtimeApi.sendMessage(data.runId, data.content))

export const resumeRun = createServerFn({ method: 'POST' })
  .validator((input: { runId: string; payload: ResumeRunRequest }) => input)
  .handler(({ data }) => runtimeApi.resumeRun(data.runId, data.payload))

export const approveRun = createServerFn({ method: 'POST' })
  .validator((runId: string) => runId)
  .handler(({ data: runId }) => runtimeApi.approveRun(runId))

export const requestChanges = createServerFn({ method: 'POST' })
  .validator((input: { runId: string; content: string }) => input)
  .handler(({ data }) => runtimeApi.requestChanges(data.runId, data.content))

export const cancelRun = createServerFn({ method: 'POST' })
  .validator((runId: string) => runId)
  .handler(({ data: runId }) => runtimeApi.cancelRun(runId))
