import { CodyLoader } from '@/components/matrix/cody/loaders'

export default function Loading() {
  return (
    <div className="bg-background flex h-svh w-full items-center justify-center">
      <CodyLoader variant="ring" label="Loading Neo…" />
    </div>
  )
}
