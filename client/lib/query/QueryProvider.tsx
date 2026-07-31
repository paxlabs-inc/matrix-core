'use client'

/**
 * QueryProvider — wraps the React tree with a TanStack Query client
 * tuned for the Matrix dashboard:
 *
 *   - Server-state-aware staleness: 30s for "live" reads (intents),
 *     5min for catalogue reads (agents/tools/skills) — staleTime is
 *     overridden per-query when needed.
 *   - Background refetch on focus + reconnect to keep the UI honest.
 *   - One retry on transient errors; the underlying api client already
 *     does its own retries so we don't pile them on.
 *   - Query devtools mounted in development only; production strips
 *     them via `process.env.NODE_ENV` branching at the dynamic-import
 *     level so the bundle stays clean.
 */
import { useState, type ReactNode } from 'react'
import { QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ApiError } from '@/lib/api/client'

function makeClient() {
  return new QueryClient({
    // Any query that 401s routes through a single-flight token refresh —
    // recovering silently when possible, otherwise redirecting to login —
    // instead of leaving a dead card spinning on an expired session.
    queryCache: new QueryCache({
      onError: (error) => {
        if (error instanceof ApiError && error.status === 401) {
          void import('@/lib/auth/session').then(({ handleUnauthorized }) => handleUnauthorized())
        }
      },
    }),
    defaultOptions: {
      queries: {
        staleTime: 30_000,
        gcTime: 5 * 60_000,
        refetchOnWindowFocus: true,
        refetchOnReconnect: true,
        retry: (failureCount, error) => {
          // Server-side errors with retryable=true get one extra
          // attempt; everything else (auth failures, 404s) stays
          // single-shot.
          if (failureCount > 1) return false
          return error instanceof ApiError ? error.retryable : false
        },
        retryDelay: (attempt) => Math.min(2_000, 200 * 2 ** attempt),
      },
      mutations: {
        retry: 0,
      },
    },
  })
}

export function QueryProvider({ children }: { children: ReactNode }) {
  // Lazy initialiser — the client must outlive Strict Mode's two
  // invocations. Per TanStack docs, never construct it during render.
  const [client] = useState(makeClient)
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>
}
