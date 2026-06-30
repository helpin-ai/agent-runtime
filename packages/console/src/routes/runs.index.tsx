import { useMemo, useState } from 'react'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useSuspenseQuery } from '@tanstack/react-query'
import { Button } from '~/components/ui/button'
import { Input } from '~/components/ui/input'
import { Select } from '~/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '~/components/ui/table'
import { RunStatusBadge } from '~/components/run-status-badge'
import { runsQuery } from '~/lib/queries'
import type { RunStatus } from '~/lib/types'

const STATUSES: Array<RunStatus> = [
  'queued',
  'running',
  'paused',
  'completed',
  'failed',
  'cancelled',
]
const PAGE_SIZE = 25

export const Route = createFileRoute('/runs/')({
  loader: ({ context }) => context.queryClient.ensureQueryData(runsQuery()),
  component: RunsList,
})

function RunsList() {
  const { data: runs } = useSuspenseQuery(runsQuery())
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState<RunStatus | 'all'>('all')
  const [limit, setLimit] = useState(PAGE_SIZE)

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    return runs.filter((run) => {
      if (status !== 'all' && run.status !== status) return false
      if (!q) return true
      return [
        run.id,
        run.agent_id,
        run.target.id,
        run.target.type,
        run.target.display?.title,
      ]
        .filter(Boolean)
        .some((v) => v!.toLowerCase().includes(q))
    })
  }, [runs, search, status])

  const visible = filtered.slice(0, limit)

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Runs</h1>
          <p className="text-muted-foreground text-sm">
            {filtered.length} of {runs.length} run{runs.length === 1 ? '' : 's'} ·
            auto-refreshing.
          </p>
        </div>
        <Link to="/runs/new">
          <Button>New run</Button>
        </Link>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <Input
          value={search}
          onChange={(e) => {
            setSearch(e.target.value)
            setLimit(PAGE_SIZE)
          }}
          placeholder="Search by id, agent, or target…"
          className="max-w-xs"
        />
        <Select
          value={status}
          onChange={(e) => {
            setStatus(e.target.value as RunStatus | 'all')
            setLimit(PAGE_SIZE)
          }}
          className="max-w-[12rem]"
        >
          <option value="all">All statuses</option>
          {STATUSES.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </Select>
      </div>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Run</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Target</TableHead>
            <TableHead>Runtime</TableHead>
            <TableHead>Mode</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {visible.length === 0 ? (
            <TableRow>
              <TableCell colSpan={5} className="text-muted-foreground py-8 text-center">
                {runs.length === 0 ? 'No runs yet.' : 'No runs match your filters.'}
              </TableCell>
            </TableRow>
          ) : (
            visible.map((run) => (
              <TableRow key={run.id}>
                <TableCell className="font-medium">
                  <Link
                    to="/runs/$runId"
                    params={{ runId: run.id }}
                    className="hover:underline"
                  >
                    {run.id}
                  </Link>
                </TableCell>
                <TableCell>
                  <RunStatusBadge status={run.status} />
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {run.target.display?.title ?? `${run.target.type}:${run.target.id}`}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {run.runtime_kind}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {run.invocation_mode}
                </TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>

      {filtered.length > limit ? (
        <div className="flex justify-center">
          <Button variant="outline" onClick={() => setLimit((n) => n + PAGE_SIZE)}>
            Load more ({filtered.length - limit} more)
          </Button>
        </div>
      ) : null}
    </div>
  )
}
