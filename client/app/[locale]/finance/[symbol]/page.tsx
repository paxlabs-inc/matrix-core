import nextDynamic from 'next/dynamic'
import type { Metadata } from 'next'
import { Suspense } from 'react'
import Loading from '../../loading'

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

export async function generateMetadata({
  params,
}: {
  params: Promise<{ symbol: string }>
}): Promise<Metadata> {
  const { symbol } = await params
  return { title: `${decodeURIComponent(symbol).toUpperCase()} — Markets` }
}

const SymbolView = nextDynamic(
  () => import('@/components/matrix/finance/finance-surface').then((m) => m.SymbolView),
  { ssr: true, loading: () => <Loading /> },
)

export default async function SymbolPage({ params }: { params: Promise<{ symbol: string }> }) {
  const { symbol } = await params
  return (
    <Suspense fallback={<Loading />}>
      <SymbolView symbol={decodeURIComponent(symbol)} />
    </Suspense>
  )
}
