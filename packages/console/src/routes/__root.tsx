/// <reference types="vite/client" />
import {
  HeadContent,
  Link,
  Scripts,
  createRootRouteWithContext,
} from '@tanstack/react-router'
import { TanStackRouterDevtools } from '@tanstack/react-router-devtools'
import { ReactQueryDevtools } from '@tanstack/react-query-devtools'
import type { QueryClient } from '@tanstack/react-query'
import { Bot, LayoutDashboard, ListChecks, SlidersHorizontal } from 'lucide-react'
import * as React from 'react'
import { DefaultCatchBoundary } from '~/components/DefaultCatchBoundary'
import { NotFound } from '~/components/NotFound'
import { cn } from '~/lib/utils'
import appCss from '~/styles/app.css?url'
import { seo } from '~/lib/seo'

export interface RouterContext {
  queryClient: QueryClient
}

export const Route = createRootRouteWithContext<RouterContext>()({
  head: () => ({
    meta: [
      { charSet: 'utf-8' },
      { name: 'viewport', content: 'width=device-width, initial-scale=1' },
      ...seo({
        title: 'Agent Runtime Console',
        description: 'Operate agents and runs on the Agent Runtime.',
      }),
    ],
    links: [{ rel: 'stylesheet', href: appCss }],
  }),
  errorComponent: DefaultCatchBoundary,
  notFoundComponent: () => <NotFound />,
  shellComponent: RootDocument,
})

const NAV = [
  { to: '/', label: 'Dashboard', icon: LayoutDashboard, exact: true },
  { to: '/agents', label: 'Agents', icon: Bot, exact: false },
  { to: '/runs', label: 'Runs', icon: ListChecks, exact: false },
  { to: '/config', label: 'Configuration', icon: SlidersHorizontal, exact: false },
] as const

function RootDocument({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className="dark">
      <head>
        <HeadContent />
      </head>
      <body>
        <div className="flex min-h-screen">
          <aside className="bg-card hidden w-60 shrink-0 border-r p-4 md:flex md:flex-col">
            <div className="mb-6 px-2">
              <p className="text-sm font-semibold">Agent Runtime</p>
              <p className="text-muted-foreground text-xs">Console</p>
            </div>
            <nav className="flex flex-col gap-1">
              {NAV.map((item) => (
                <Link
                  key={item.to}
                  to={item.to}
                  activeOptions={{ exact: item.exact }}
                  className="text-muted-foreground hover:bg-accent hover:text-accent-foreground flex items-center gap-2 rounded-md px-2 py-1.5 text-sm font-medium transition-colors"
                  activeProps={{
                    className: cn('bg-accent text-accent-foreground'),
                  }}
                >
                  <item.icon className="size-4" />
                  {item.label}
                </Link>
              ))}
            </nav>
          </aside>
          <main className="min-w-0 flex-1 p-6">{children}</main>
        </div>
        {import.meta.env.DEV ? (
          <>
            <TanStackRouterDevtools position="bottom-right" />
            <ReactQueryDevtools buttonPosition="bottom-left" />
          </>
        ) : null}
        <Scripts />
      </body>
    </html>
  )
}
