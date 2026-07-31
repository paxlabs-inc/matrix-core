import nextDynamic from 'next/dynamic'
import { Suspense } from 'react'
import { listPresetSummaries } from '@/lib/data/presets.server'

const CodeGallery = nextDynamic(
  () => import('@/components/matrix/code/code-gallery').then((m) => m.CodeGallery),
  {
    loading: () => (
      <div className="bg-background flex h-svh w-full items-center justify-center">
        <span className="text-muted-foreground text-sm">Loading templates…</span>
      </div>
    ),
  },
)

export default function CodePage() {
  const presets = listPresetSummaries()
  return (
    <Suspense
      fallback={
        <div className="bg-background flex h-svh w-full items-center justify-center">
          <span className="text-muted-foreground text-sm">Loading templates…</span>
        </div>
      }
    >
      <CodeGallery initialPresets={presets} />
    </Suspense>
  )
}
