/// <reference types="vite/client" />
import {
  HeadContent,
  Link,
  Scripts,
  createRootRouteWithContext,
  retainSearchParams,
  useRouterState,
} from '@tanstack/react-router'
import { TanStackRouterDevtools } from '@tanstack/react-router-devtools'
import { ReactQueryDevtools } from '@tanstack/react-query-devtools'
import type { QueryClient } from '@tanstack/react-query'
import { Bot, LayoutDashboard, ListChecks, SlidersHorizontal } from 'lucide-react'
import * as React from 'react'
import { DefaultCatchBoundary } from '~/components/DefaultCatchBoundary'
import { NotFound } from '~/components/NotFound'
import { cn } from '~/lib/utils'
import { AppScopeProvider, useAppScope } from '~/lib/app-scope'
import { appCatalogQuery } from '~/lib/queries'
import appCss from '~/styles/app.css?url'
import { seo } from '~/lib/seo'

export interface RouterContext {
  queryClient: QueryClient
}

export const Route = createRootRouteWithContext<RouterContext>()({
  validateSearch: (search: Record<string, unknown>): { app?: string } => ({
    app: typeof search.app === 'string' && search.app.trim() ? search.app.trim() : undefined,
  }),
  search: {
    middlewares: [retainSearchParams(['app'])],
  },
  beforeLoad: async ({ context, search }) => {
    const catalog = await context.queryClient.ensureQueryData(appCatalogQuery())
    const apps = catalog.apps.length > 0
      ? catalog.apps
      : [{ app_id: catalog.default_app_id, components: [] }]
    const appId = apps.some((app) => app.app_id === search.app)
      ? search.app!
      : apps.some((app) => app.app_id === catalog.default_app_id)
        ? catalog.default_app_id
        : apps[0].app_id
    return { appId, apps }
  },
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
  const { appId, apps } = Route.useRouteContext()
  const navigate = Route.useNavigate()
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  const selectApp = React.useCallback((nextAppId: string) => {
    if (nextAppId === appId || !apps.some((app) => app.app_id === nextAppId)) return
    const destination = pathname.startsWith('/runs/')
      ? '/runs'
      : pathname.startsWith('/agents/')
        ? '/agents'
        : null
    React.startTransition(() => {
      const search = () => ({ app: nextAppId })
      if (destination) {
        void navigate({ to: destination, search })
      } else {
        void navigate({ search })
      }
    })
  }, [appId, apps, navigate, pathname])
  return (
    <html lang="en" className="dark">
      <head>
        <HeadContent />
      </head>
      <body>
        <AppScopeProvider appId={appId} apps={apps} selectApp={selectApp}>
          <div className="flex min-h-screen">
            <Sidebar />
            <main key={appId} className="min-w-0 flex-1 p-4 md:p-6">
              <div className="mb-4 md:hidden"><AppSelector /></div>
              {children}
            </main>
          </div>
        </AppScopeProvider>
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

function Sidebar() {
  return (
    <aside className="bg-card hidden w-60 shrink-0 border-r p-4 md:flex md:flex-col">
      <div className="mb-5 px-2">
        <p className="text-sm font-semibold">Agent Runtime</p>
        <p className="text-muted-foreground text-xs">Console</p>
      </div>
      <AppSelector />
      <nav className="mt-4 flex flex-col gap-1 border-t pt-4">
        {NAV.map((item) => (
          <Link
            key={item.to}
            to={item.to}
            activeOptions={{ exact: item.exact }}
            className="text-muted-foreground hover:bg-accent hover:text-accent-foreground flex items-center gap-2 rounded-md px-2 py-1.5 text-sm font-medium transition-colors"
            activeProps={{ className: cn('bg-accent text-accent-foreground') }}
          >
            <item.icon className="size-4" />
            {item.label}
          </Link>
        ))}
      </nav>
    </aside>
  )
}

function AppSelector() {
  const { appId, apps, selectApp } = useAppScope()
  return (
    <label className="block px-2 text-xs text-muted-foreground">
      Application
      <select
        value={appId}
        onChange={(event) => selectApp(event.target.value)}
        className="border-input bg-background text-foreground mt-1.5 h-9 w-full rounded-md border px-2 text-sm"
      >
        {apps.map((app) => (
          <option key={app.app_id} value={app.app_id}>{app.app_id}</option>
        ))}
      </select>
    </label>
  )
}
