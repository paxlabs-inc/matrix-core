'use client'

import { lazy, Suspense, type ReactNode } from 'react'
import { usePathname } from 'next/navigation'
import { QueryProvider } from '@/lib/query/QueryProvider'

const DefaultAppProviders = lazy(async () => {
  const providers = await import('./DefaultAppProviders')
  return { default: providers.DefaultAppProviders }
})

export function isWorkforceRoute(pathname: string, locale: string): boolean {
  const route = `/${locale}/workforce`
  return pathname === route || pathname.startsWith(`${route}/`)
}

export function AppProviders({ children, locale }: { children: ReactNode; locale: string }) {
  const pathname = usePathname()
  if (isWorkforceRoute(pathname, locale)) {
    return <QueryProvider>{children}</QueryProvider>
  }

  return (
    <Suspense fallback={null}>
      <DefaultAppProviders locale={locale}>{children}</DefaultAppProviders>
    </Suspense>
  )
}
