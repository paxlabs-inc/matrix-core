'use client'

/**
 * AppProviders — single client-boundary wrapper that mounts every
 * cross-cutting context (auth, query cache, theme). Keeping this as one
 * component lets the root layout stay a server component and ship the
 * minimum amount of JavaScript above the dashboard tree.
 */
import { type ReactNode } from 'react'
import { ThemeProvider } from '@/components/theme-provider'
import { DesignModeProvider } from '@/components/design'
import { AuthProvider } from '@/lib/auth/AuthProvider'
import { QueryProvider } from '@/lib/query/QueryProvider'

export function AppProviders({ children }: { children: ReactNode }) {
  return (
    <ThemeProvider
      attribute="class"
      defaultTheme="dark"
      enableSystem={false}
      disableTransitionOnChange
    >
      <QueryProvider>
        <AuthProvider>
          <DesignModeProvider>{children}</DesignModeProvider>
        </AuthProvider>
      </QueryProvider>
    </ThemeProvider>
  )
}
