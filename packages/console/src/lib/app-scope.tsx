import { createContext, useContext } from 'react'
import type { ReactNode } from 'react'
import type { AppSummary } from './types'

interface AppScopeValue {
  appId: string
  apps: Array<AppSummary>
  selectApp: (appId: string) => void
}

const AppScopeContext = createContext<AppScopeValue | null>(null)

export function AppScopeProvider({
  appId,
  apps,
  selectApp,
  children,
}: {
  appId: string
  apps: Array<AppSummary>
  selectApp: (appId: string) => void
  children: ReactNode
}) {
  return (
    <AppScopeContext.Provider value={{ appId, apps, selectApp }}>
      {children}
    </AppScopeContext.Provider>
  )
}

export function useAppScope(): AppScopeValue {
  const value = useContext(AppScopeContext)
  if (!value) throw new Error('useAppScope must be used inside AppScopeProvider')
  return value
}
