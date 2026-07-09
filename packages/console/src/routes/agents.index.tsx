import { useMemo, useState } from 'react'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useSuspenseQuery } from '@tanstack/react-query'
import { Badge } from '~/components/ui/badge'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '~/components/ui/table'
import { agentsQuery } from '~/lib/queries'
import { useAppScope } from '~/lib/app-scope'

export const Route = createFileRoute('/agents/')({
  loader: ({ context }) => context.queryClient.ensureQueryData(agentsQuery(context.appId)),
  component: AgentsList,
})

function AgentsList() {
  const { appId } = useAppScope()
  const { data: allAgents } = useSuspenseQuery(agentsQuery(appId))
  const [search, setSearch] = useState('')

  const agents = useMemo(() => {
    const q = search.trim().toLowerCase()
    if (!q) return allAgents
    return allAgents.filter((a) =>
      [a.name, a.id, a.runtime_kind, a.model]
        .filter(Boolean)
        .some((v) => v!.toLowerCase().includes(q)),
    )
  }, [allAgents, search])

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Agents</h1>
          <p className="text-muted-foreground text-sm">
            {agents.length} of {allAgents.length} agent
            {allAgents.length === 1 ? '' : 's'}.
          </p>
        </div>
        <Link to="/agents/new">
          <Button>New agent</Button>
        </Link>
      </div>
      <Input
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        placeholder="Search agents…"
        className="max-w-xs"
      />
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Runtime</TableHead>
            <TableHead>Model</TableHead>
            <TableHead>Approval</TableHead>
            <TableHead>Tools</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {agents.length === 0 ? (
            <TableRow>
              <TableCell colSpan={5} className="text-muted-foreground py-8 text-center">
                {allAgents.length === 0 ? 'No agents yet.' : 'No agents match.'}
              </TableCell>
            </TableRow>
          ) : (
            agents.map((agent) => (
              <TableRow key={agent.id ?? agent.name}>
                <TableCell className="font-medium">
                  {agent.id ? (
                    <Link
                      to="/agents/$agentId"
                      params={{ agentId: agent.id }}
                      className="hover:underline"
                    >
                      {agent.name}
                    </Link>
                  ) : (
                    agent.name
                  )}
                </TableCell>
                <TableCell>
                  {agent.runtime_kind ? (
                    <Badge variant="secondary">{agent.runtime_kind}</Badge>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {agent.model ?? '—'}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {agent.approval_mode ?? 'never'}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {agent.allowed_tools?.length ?? 0}
                </TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>
    </div>
  )
}
