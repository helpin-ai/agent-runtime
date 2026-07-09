import { useState } from 'react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useMutation, useSuspenseQuery } from '@tanstack/react-query'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '~/components/ui/card'
import { Input } from '~/components/ui/input'
import { Label } from '~/components/ui/label'
import { Select } from '~/components/ui/select'
import { Textarea } from '~/components/ui/textarea'
import { agentsQuery, systemConfigQuery } from '~/lib/queries'
import { startRun } from '~/lib/runtime-fns'
import type { StartRunRequest } from '~/lib/types'

export const Route = createFileRoute('/runs/new')({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(agentsQuery()),
      context.queryClient.ensureQueryData(systemConfigQuery()),
    ])
  },
  component: NewRun,
})

function NewRun() {
  const navigate = useNavigate()
  const { data: agents } = useSuspenseQuery(agentsQuery())
  const { data: config } = useSuspenseQuery(systemConfigQuery())

  const [agentId, setAgentId] = useState(agents[0]?.id ?? '')
  const [targetType, setTargetType] = useState('task')
  const [targetId, setTargetId] = useState('')
  const [instructions, setInstructions] = useState('')
  const [mode, setMode] = useState<'autonomous' | 'interactive'>('autonomous')
  const [executionMode, setExecutionMode] = useState<'lightweight' | 'durable'>(
    config.capabilities.durable.enabled ? 'durable' : 'lightweight',
  )

  const launch = useMutation({
    mutationFn: () => {
      const req: StartRunRequest = {
        app_id: config.connection.app_id,
        agent_id: agentId,
        target: { type: targetType.trim(), id: targetId.trim() },
        instructions: instructions.trim() || undefined,
        mode,
        execution_mode: executionMode,
      }
      return startRun({ data: req })
    },
    onSuccess: (run) => navigate({ to: '/runs/$runId', params: { runId: run.id } }),
  })

  const canSubmit = agentId && targetType.trim() && targetId.trim()

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Start a run</h1>
        <p className="text-muted-foreground text-sm">
          Launch an agent against a target.
        </p>
      </div>

      {agents.length === 0 ? (
        <Card>
          <CardContent className="text-muted-foreground py-6 text-sm">
            No agents exist yet. Create one first.
          </CardContent>
        </Card>
      ) : (
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            launch.mutate()
          }}
        >
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Target</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              <div className="space-y-1.5">
                <Label htmlFor="agent">Agent</Label>
                <Select
                  id="agent"
                  value={agentId}
                  onChange={(e) => setAgentId(e.target.value)}
                >
                  {agents.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.name}
                      {a.runtime_kind ? ` (${a.runtime_kind})` : ''}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <Label htmlFor="ttype">Target type</Label>
                  <Input
                    id="ttype"
                    value={targetType}
                    onChange={(e) => setTargetType(e.target.value)}
                    placeholder="task"
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="tid">Target id</Label>
                  <Input
                    id="tid"
                    value={targetId}
                    onChange={(e) => setTargetId(e.target.value)}
                    placeholder="task_123"
                  />
                </div>
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-base">Instructions & mode</CardTitle>
            </CardHeader>
            <CardContent className="space-y-3">
              <div className="space-y-1.5">
                <Label htmlFor="instr">Instructions</Label>
                <Textarea
                  id="instr"
                  value={instructions}
                  onChange={(e) => setInstructions(e.target.value)}
                  placeholder="What should the agent do?"
                  className="min-h-32"
                />
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <Label htmlFor="mode">Invocation mode</Label>
                  <Select
                    id="mode"
                    value={mode}
                    onChange={(e) =>
                      setMode(e.target.value as 'autonomous' | 'interactive')
                    }
                  >
                    <option value="autonomous">autonomous</option>
                    <option value="interactive">interactive</option>
                  </Select>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="exec">Execution mode</Label>
                  <Select
                    id="exec"
                    value={executionMode}
                    onChange={(e) =>
                      setExecutionMode(e.target.value as 'lightweight' | 'durable')
                    }
                  >
                    <option value="lightweight">lightweight</option>
                    <option value="durable" disabled={!config.capabilities.durable.enabled}>
                      durable{config.capabilities.durable.enabled ? '' : ' — Temporal unavailable'}
                    </option>
                  </Select>
                </div>
              </div>
            </CardContent>
          </Card>

          {launch.isError ? (
            <p className="text-destructive text-sm">
              {(launch.error as Error).message}
            </p>
          ) : null}

          <div className="flex gap-2">
            <Button type="submit" disabled={!canSubmit || launch.isPending}>
              {launch.isPending ? 'Starting…' : 'Start run'}
            </Button>
            <Button
              type="button"
              variant="outline"
              onClick={() => navigate({ to: '/runs' })}
            >
              Cancel
            </Button>
          </div>
        </form>
      )}
    </div>
  )
}
