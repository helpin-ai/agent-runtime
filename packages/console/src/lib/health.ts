import type { Agent, AgentRun, Capabilities } from './types'

export interface AttentionItem {
  id: string
  severity: 'error' | 'warn'
  title: string
  detail?: string
  runId?: string
}

// Default native-SDK provider when an agent leaves `provider` unset (matches the
// runtime, which falls back to anthropic).
const DEFAULT_PROVIDER = 'anthropic'

export function agentEffectiveProvider(agent: Agent): string {
  return agent.provider?.trim() || DEFAULT_PROVIDER
}

// Surfaces operator action items: runs awaiting approval, failed runs, and
// agents wired to a model provider whose key isn't configured.
export function deriveAttention(
  runs: Array<AgentRun>,
  agents: Array<Agent>,
  capabilities: Capabilities,
): Array<AttentionItem> {
  const items: Array<AttentionItem> = []

  for (const run of runs) {
    if (run.status === 'paused') {
      items.push({
        id: `approval-${run.id}`,
        severity: 'warn',
        title: 'Run awaiting approval',
        detail: run.pause_reason ?? run.target.display?.title ?? run.id,
        runId: run.id,
      })
    } else if (run.status === 'failed') {
      items.push({
        id: `failed-${run.id}`,
        severity: 'error',
        title: 'Run failed',
        detail: run.error_message ?? run.id,
        runId: run.id,
      })
    }
  }

  const configured = new Set(
    capabilities.providers.filter((p) => p.configured).map((p) => p.name),
  )
  for (const agent of agents) {
    if (agent.runtime_kind && agent.runtime_kind !== 'native_sdk') continue
    const provider = agentEffectiveProvider(agent)
    if (!configured.has(provider)) {
      items.push({
        id: `misconfig-${agent.id ?? agent.name}`,
        severity: 'error',
        title: `Agent "${agent.name}" uses an unconfigured provider`,
        detail: `Provider "${provider}" has no API key set on the runtime.`,
      })
    }
  }

  // Errors first, then warnings.
  return items.sort((a, b) =>
    a.severity === b.severity ? 0 : a.severity === 'error' ? -1 : 1,
  )
}
