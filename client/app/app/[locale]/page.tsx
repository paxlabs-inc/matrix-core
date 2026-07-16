import nextDynamic from 'next/dynamic'
import { Suspense } from 'react'
import Loading from './loading'
import { OnboardingGate } from '@/components/matrix/onboarding/onboarding-gate'

/**
 * Authenticated dashboard — intentionally dynamic (SSR per request).
 * Reads locale cookies in the root layout and is gated by the edge proxy.
 */
export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

const Dashboard = nextDynamic(
  () => import('@/components/matrix/dashboard').then((m) => m.Dashboard),
  {
    ssr: true,
    loading: () => <Loading />,
  },
)

export default function Page() {
  return (
    <Suspense fallback={<Loading />}>
      <OnboardingGate>
        <Dashboard />
      </OnboardingGate>
    </Suspense>
  )
}
