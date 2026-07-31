'use client'

/**
 * ProvisioningGate — holds back the dashboard for an authenticated user
 * until their daemon machine is reachable, so a brand-new user never sees
 * the static mock fixtures the dashboard falls back to while data is
 * absent. It probes GET /me through the router (which provisions / wakes
 * the user's machine on first contact) and polls until it answers, then
 * reveals the children.
 *
 * Returning users with a warm machine clear this near-instantly and skip
 * the intro video (tracked via the `mx-onboarded` flag).
 */
import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useAuth } from '@/lib/auth/AuthProvider'
import { getMe } from '@/lib/api/identity'
import { ProvisioningScreen } from '@/components/matrix/provisioning-screen'

const ONBOARDED_KEY = 'mx-onboarded'

export function ProvisioningGate({ children }: { children: React.ReactNode }) {
  const { enabled, loading: authLoading, user } = useAuth()

  const [firstTime] = useState<boolean>(() => {
    if (typeof window === 'undefined') return false
    return window.localStorage.getItem(ONBOARDED_KEY) == null
  })

  // Readiness probe. Only runs once authenticated. We drive retries via
  // refetchInterval (not React Query's retry) so a long provisioning
  // window keeps polling instead of giving up.
  const probe = useQuery({
    queryKey: ['provision-probe'],
    queryFn: ({ signal }) => getMe(signal),
    enabled: enabled && !!user,
    retry: false,
    refetchOnWindowFocus: false,
    staleTime: 30_000,
    refetchInterval: (query) => (query.state.status === 'success' ? false : 3500),
  })

  const ready = !enabled || (!!user && probe.isSuccess)

  // Remember that this browser has finished onboarding so the intro video
  // only plays on the very first sign-in.
  useEffect(() => {
    if (ready && typeof window !== 'undefined') {
      try {
        window.localStorage.setItem(ONBOARDED_KEY, '1')
      } catch {
        /* private mode / quota — non-fatal */
      }
    }
  }, [ready])

  // Auth not configured (local dev) — nothing to provision.
  if (!enabled) return <>{children}</>

  // Still resolving the session: keep it quiet to avoid a flash of the
  // provisioning screen for users who actually have a warm machine.
  if (authLoading) {
    return (
      <main className="bg-background fixed inset-0 z-50 flex items-center justify-center">
        <div
          className="border-muted-foreground/30 border-t-primary size-6 animate-spin rounded-full border-2"
          aria-hidden
        />
      </main>
    )
  }

  // Unauthenticated: the edge proxy redirects to /login; render children
  // so the normal flow (and any client-side guard) takes over.
  if (!user) return <>{children}</>

  if (ready) return <>{children}</>

  return <ProvisioningScreen firstTime={firstTime} />
}
