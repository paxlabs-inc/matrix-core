import nextDynamic from 'next/dynamic'
import type { Metadata } from 'next'
import { Suspense } from 'react'
import Loading from '../loading'

/**
 * The market surface — authenticated like the rest of the app, dynamic per
 * request. All of its data comes from the router's finance lane, so no vendor
 * key or vendor script is ever part of this bundle.
 */
export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

export const metadata: Metadata = {
  title: 'Markets',
}

const MarketsHomeView = nextDynamic(
  () => import('@/components/matrix/finance/finance-surface').then((m) => m.MarketsHomeView),
  { ssr: true, loading: () => <Loading /> },
)

export default function FinancePage() {
  return (
    <Suspense fallback={<Loading />}>
      <MarketsHomeView />
    </Suspense>
  )
}
