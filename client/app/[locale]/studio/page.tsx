import nextDynamic from 'next/dynamic'
import type { Metadata } from 'next'
import { Suspense } from 'react'
import Loading from '../loading'

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

export const metadata: Metadata = {
  title: 'Image & Video Studio',
}

const MediaStudio = nextDynamic(
  () =>
    import('@/components/matrix/media-studio/media-studio').then((module) => module.MediaStudio),
  { ssr: true, loading: () => <Loading /> },
)

export default function StudioPage() {
  return (
    <Suspense fallback={<Loading />}>
      <MediaStudio />
    </Suspense>
  )
}
