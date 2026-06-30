import type { ReactNode } from 'react'
import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { Button } from '~/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '~/components/ui/card'
import { Input } from '~/components/ui/input'
import { Label } from '~/components/ui/label'
import { Select } from '~/components/ui/select'
import { Textarea } from '~/components/ui/textarea'
import { createAgent, updateAgent } from '~/lib/runtime-fns'
import type { Agent, Capabilities, RuntimeKind } from '~/lib/types'

const RUNTIME_KINDS: Array<RuntimeKind> = ['native_sdk', 'codex', 'opencode']

// Comma/space/newline separated string <-> string[] for the list fields.
function toList(value: string): Array<string> {
  return value
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean)
}

export function AgentForm({
  appId,
  initial,
  capabilities,
}: {
  appId: string
  initial?: Agent
  capabilities: Capabilities
}) {
  const navigate = useNavigate()
  const editing = Boolean(initial?.id)

  const [name, setName] = useState(initial?.name ?? '')
  const [runtimeKind, setRuntimeKind] = useState<RuntimeKind>(
    initial?.runtime_kind ?? 'native_sdk',
  )
  const [provider, setProvider] = useState(initial?.provider ?? '')
  const [model, setModel] = useState(initial?.model ?? '')
  const [systemPrompt, setSystemPrompt] = useState(initial?.system_prompt ?? '')
  const [approvalMode, setApprovalMode] = useState(
    initial?.approval_mode ?? 'never',
  )
  const [invocationMode, setInvocationMode] = useState(
    initial?.default_invocation_mode ?? 'autonomous',
  )
  const [allowedTools, setAllowedTools] = useState<Array<string>>(
    initial?.allowed_tools ?? [],
  )
  const [allowedTargets, setAllowedTargets] = useState(
    (initial?.allowed_targets ?? []).join(', '),
  )

  const save = useMutation({
    mutationFn: () => {
      const agent: Agent = {
        ...initial,
        app_id: appId,
        name: name.trim(),
        runtime_kind: runtimeKind,
        provider: provider.trim() || undefined,
        model: model.trim() || undefined,
        system_prompt: systemPrompt.trim() || undefined,
        approval_mode: approvalMode as Agent['approval_mode'],
        default_invocation_mode:
          invocationMode as Agent['default_invocation_mode'],
        allowed_tools: allowedTools,
        allowed_targets: toList(allowedTargets),
      }
      return editing && initial?.id
        ? updateAgent({ data: { agentId: initial.id, agent } })
        : createAgent({ data: agent })
    },
    onSuccess: (saved) => {
      navigate({
        to: '/agents/$agentId',
        params: { agentId: saved.id ?? initial?.id ?? '' },
      })
    },
  })

  function toggleTool(tool: string) {
    setAllowedTools((prev) =>
      prev.includes(tool) ? prev.filter((t) => t !== tool) : [...prev, tool],
    )
  }

  return (
    <form
      className="space-y-4"
      onSubmit={(e) => {
        e.preventDefault()
        save.mutate()
      }}
    >
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Identity & model</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <Field label="Name" htmlFor="name">
              <Input
                id="name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="Support Triage Agent"
                required
              />
            </Field>
            <Field label="Runtime" htmlFor="runtime">
              <Select
                id="runtime"
                value={runtimeKind}
                onChange={(e) => setRuntimeKind(e.target.value as RuntimeKind)}
              >
                {RUNTIME_KINDS.map((k) => (
                  <option key={k} value={k}>
                    {k}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Provider" htmlFor="provider">
              <Select
                id="provider"
                value={provider}
                onChange={(e) => setProvider(e.target.value)}
              >
                <option value="">(runtime default)</option>
                {capabilities.providers.map((p) => (
                  <option key={p.name} value={p.name}>
                    {p.name}
                    {p.configured ? '' : ' — no key set'}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Model" htmlFor="model">
              <Input
                id="model"
                value={model}
                onChange={(e) => setModel(e.target.value)}
                placeholder="(provider default)"
              />
            </Field>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Behavior</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            <Field label="Approval mode" htmlFor="approval">
              <Select
                id="approval"
                value={approvalMode}
                onChange={(e) => setApprovalMode(e.target.value as 'never' | 'always')}
              >
                <option value="never">never</option>
                <option value="always">always</option>
              </Select>
            </Field>
            <Field label="Default invocation" htmlFor="invocation">
              <Select
                id="invocation"
                value={invocationMode}
                onChange={(e) =>
                  setInvocationMode(e.target.value as 'autonomous' | 'interactive')
                }
              >
                <option value="autonomous">autonomous</option>
                <option value="interactive">interactive</option>
              </Select>
            </Field>
            <Field label="Allowed targets" htmlFor="targets">
              <Input
                id="targets"
                value={allowedTargets}
                onChange={(e) => setAllowedTargets(e.target.value)}
                placeholder="task, document (comma separated)"
              />
            </Field>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">System prompt</CardTitle>
        </CardHeader>
        <CardContent>
          <Textarea
            value={systemPrompt}
            onChange={(e) => setSystemPrompt(e.target.value)}
            placeholder="You are a helpful agent that…"
            className="min-h-40"
          />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">
            Allowed tools{' '}
            <span className="text-muted-foreground text-sm font-normal">
              ({allowedTools.length} selected)
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent className="grid gap-1.5 sm:grid-cols-2 lg:grid-cols-3">
          {capabilities.tools.map((t) => (
            <label
              key={t.name}
              className="hover:bg-accent flex items-center gap-2 rounded-md p-1.5 text-sm"
            >
              <input
                type="checkbox"
                checked={allowedTools.includes(t.name)}
                onChange={() => toggleTool(t.name)}
              />
              <span className="font-mono text-xs">{t.name}</span>
            </label>
          ))}
        </CardContent>
      </Card>

      {save.isError ? (
        <p className="text-destructive text-sm">
          {(save.error as Error).message}
        </p>
      ) : null}

      <div className="flex gap-2">
        <Button type="submit" disabled={!name.trim() || save.isPending}>
          {save.isPending ? 'Saving…' : editing ? 'Save changes' : 'Create agent'}
        </Button>
        <Button
          type="button"
          variant="outline"
          onClick={() => navigate({ to: '/agents' })}
        >
          Cancel
        </Button>
      </div>
    </form>
  )
}

function Field({
  label,
  htmlFor,
  children,
}: {
  label: string
  htmlFor: string
  children: ReactNode
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
    </div>
  )
}
