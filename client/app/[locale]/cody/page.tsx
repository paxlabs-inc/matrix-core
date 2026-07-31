import nextDynamic from 'next/dynamic'
import { Suspense } from 'react'

import { getPresetPrompt } from '@/lib/data/presets.server'
import Loading from './loading'

/**
 * The /cody sibling app — its own shell (sidebar, top bar, tier-driven
 * viewport). Dynamic (SSR per request) and behind the same edge auth gate as
 * the dashboard; the Neo dashboard route is untouched.
 */
export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

const CodyApp = nextDynamic(
  () => import('@/components/matrix/cody/cody-app').then((m) => m.CodyApp),
  {
    ssr: true,
    loading: () => <Loading />,
  },
)

export default async function CodyPage({
  searchParams,
}: {
  searchParams: Promise<{ preset?: string }>
}) {
  const { preset } = await searchParams
  const presetPrompt = getPresetPrompt(preset)
  return (
    <Suspense fallback={<Loading />}>
      <CodyApp initialPresetPrompt={presetPrompt} />
    </Suspense>
  )
}
