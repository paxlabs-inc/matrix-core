'use client'

/**
 * OnboardingGate — decides whether to show the onboarding flow or the
 * dashboard. Checks the `mx-onboarded` localStorage flag and the provision
 * status. If the user hasn't onboarded, shows the OnboardingFlow. If they
 * have, shows the dashboard children (after a readiness probe so the
 * daemon is warm — reusing the ProvisioningGate pattern).
 */
import { useState } from 'react'
import { useAuth } from '@/lib/auth/AuthProvider'
import { getMe } from '@/lib/api/identity'
import { OnboardingFlow } from '@/components/matrix/onboarding/onboarding-flow'
import { ProvisioningScreen } from '@/components/matrix/provisioning-screen'
import { useQuery } from '@tanstack/react-query'

const ONBOARDED_KEY = 'mx-onboarded'

export function OnboardingGate({ children }: { children: React.ReactNode }) {
  const { enabled, loading: authLoading, user } = useAuth()
  const [firstTime, setFirstTime] = useState<boolean>(() => {
    if (typeof window === 'undefined') return false
    return window.localStorage.getItem(ONBOARDED_KEY) == null
  })

  const probe = useQuery({
    queryKey: ['provision-probe'],
    queryFn: ({ signal }) => getMe(signal),
    enabled: enabled && !!user && !firstTime,
    retry: false,
    refetchOnWindowFocus: false,
    staleTime: 30_000,
    refetchInterval: (query) => (query.state.status === 'success' ? false : 3500),
  })

  if (!enabled) return <>{children}</>

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

  if (!user) return <>{children}</>

  if (firstTime) {
    // On completion the flow has set the onboarded flag; flip local state
    // so the gate falls through to the readiness probe and then the
    // dashboard children, without needing a full page reload.
    return <OnboardingFlow onComplete={() => setFirstTime(false)} />
  }

  if (!probe.isSuccess) {
    return <ProvisioningScreen firstTime={false} />
  }

  return <>{children}</>
}
