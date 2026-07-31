import { Skeleton } from '@/components/ui/skeleton'

export function StatCardSkeleton() {
  return (
    <div className="border-border bg-card rounded-xl border p-4">
      <Skeleton className="mb-3 h-3 w-20" />
      <Skeleton className="h-7 w-14" />
      <Skeleton className="mt-2 h-3 w-28" />
    </div>
  )
}

export function RunCardSkeleton() {
  return (
    <div className="border-border bg-card space-y-3 rounded-xl border p-4">
      <div className="flex items-center gap-3">
        <Skeleton className="size-8 rounded-full" />
        <div className="flex-1 space-y-1.5">
          <Skeleton className="h-4 w-48" />
          <Skeleton className="h-3 w-32" />
        </div>
        <Skeleton className="h-6 w-16 rounded-full" />
      </div>
      <Skeleton className="h-1.5 w-full rounded-full" />
      <div className="flex gap-2">
        {[0, 1, 2, 3].map((i) => (
          <Skeleton key={i} className="h-3 w-12" />
        ))}
      </div>
    </div>
  )
}

export function TableRowSkeleton({ rows = 5 }: { rows?: number }) {
  return (
    <div className="divide-border divide-y">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="flex items-center gap-4 px-4 py-3">
          <Skeleton className="h-4 w-24" />
          <Skeleton className="h-4 flex-1" />
          <Skeleton className="h-4 w-20" />
          <Skeleton className="h-6 w-16 rounded-full" />
        </div>
      ))}
    </div>
  )
}

export function AgentCardSkeleton() {
  return (
    <div className="border-border bg-card space-y-3 rounded-xl border p-4">
      <div className="flex items-center gap-3">
        <Skeleton className="size-10 rounded-lg" />
        <div className="flex-1 space-y-1.5">
          <Skeleton className="h-4 w-32" />
          <Skeleton className="h-3 w-48" />
        </div>
      </div>
      <div className="flex gap-2">
        {[0, 1, 2].map((i) => (
          <Skeleton key={i} className="h-5 w-16 rounded-full" />
        ))}
      </div>
    </div>
  )
}

export function OverviewPanelSkeleton() {
  return (
    <div className="flex flex-col gap-6">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <StatCardSkeleton key={i} />
        ))}
      </div>
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        {[0, 1, 2].map((i) => (
          <RunCardSkeleton key={i} />
        ))}
      </div>
    </div>
  )
}

export function PanelSkeleton() {
  return (
    <div className="flex flex-col gap-4 p-4">
      <Skeleton className="h-6 w-40" />
      <Skeleton className="h-32 w-full rounded-xl" />
      <Skeleton className="h-32 w-full rounded-xl" />
    </div>
  )
}

export function WalletPanelSkeleton() {
  return (
    <div className="flex flex-col gap-6">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
        {[0, 1, 2].map((i) => (
          <StatCardSkeleton key={i} />
        ))}
      </div>
      <TableRowSkeleton rows={6} />
    </div>
  )
}
