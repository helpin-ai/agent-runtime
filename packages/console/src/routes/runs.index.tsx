import { useDeferredValue, useState } from 'react'
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
import { runSearchQuery } from '~/lib/queries'
import type { RunStatus } from '~/lib/types'
import { useAppScope } from '~/lib/app-scope'

const STATUSES: Array<RunStatus> = [
  'queued',
  'running',
  'paused',
  'completed',
  'failed',
  'cancelled',
]
const PAGE_SIZE = 25
const DEFAULT_SEARCH = { limit: PAGE_SIZE, offset: 0 } as const

export const Route = createFileRoute('/runs/')({
  loader: ({ context }) =>
    context.queryClient.ensureQueryData(runSearchQuery(context.appId, DEFAULT_SEARCH)),
  component: RunsList,
})

function RunsList() {
  const { appId } = useAppScope()
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState<RunStatus | 'all'>('all')
  const [offset, setOffset] = useState(0)
  const deferredSearch = useDeferredValue(search.trim())
  const { data: page } = useSuspenseQuery(
    runSearchQuery(appId, {
      q: deferredSearch || undefined,
      status: status === 'all' ? undefined : status,
      limit: PAGE_SIZE,
      offset,
    }),
  )

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Runs</h1>
          <p className="text-muted-foreground text-sm">
            {page.total} run{page.total === 1 ? '' : 's'}
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
            setOffset(0)
          }}
          placeholder="Search by id, agent, or target…"
          className="max-w-xs"
        />
        <Select
          value={status}
          onChange={(e) => {
            setStatus(e.target.value as RunStatus | 'all')
            setOffset(0)
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
          {page.items.length === 0 ? (
            <TableRow>
              <TableCell colSpan={5} className="text-muted-foreground py-8 text-center">
                {page.total === 0 && !search && status === 'all'
                  ? 'No runs yet.'
                  : 'No runs match your filters.'}
              </TableCell>
            </TableRow>
          ) : (
            page.items.map((run) => (
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

      <div className="flex items-center justify-between border-t pt-3 text-sm">
        <span className="text-muted-foreground">
          {page.total === 0
            ? 'No results'
            : `${page.offset + 1}–${Math.min(page.offset + page.items.length, page.total)} of ${page.total}`}
        </span>
        <div className="flex gap-2">
          <Button
            variant="outline"
            size="sm"
            onClick={() => setOffset((value) => Math.max(0, value - PAGE_SIZE))}
            disabled={page.offset === 0}
          >
            Previous
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => setOffset((value) => value + PAGE_SIZE)}
            disabled={page.offset + page.items.length >= page.total}
          >
            Next
          </Button>
        </div>
      </div>
    </div>
  )
}
