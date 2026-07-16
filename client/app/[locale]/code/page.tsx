import nextDynamic from 'next/dynamic'
import { Suspense } from 'react'

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
  return (
    <Suspense
      fallback={
        <div className="bg-background flex h-svh w-full items-center justify-center">
          <span className="text-muted-foreground text-sm">Loading templates…</span>
        </div>
      }
    >
      <CodeGallery />
    </Suspense>
  )
}
