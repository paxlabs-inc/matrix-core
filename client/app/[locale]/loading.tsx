import { Skeleton } from '@/components/ui/skeleton'

export default function Loading() {
  return (
    <div
      className="flex min-h-screen flex-col gap-6 p-4 md:p-6"
      role="status"
      aria-label="Loading Matrix dashboard"
    >
      {/* Header skeleton */}
      <div className="border-border flex h-14 items-center gap-3 border-b px-4">
        <Skeleton className="h-5 w-5 rounded" />
        <Skeleton className="h-4 w-24" />
        <div className="ml-auto flex gap-2">
          <Skeleton className="h-9 w-40 rounded-md" />
          <Skeleton className="h-9 w-9 rounded-md" />
          <Skeleton className="h-9 w-24 rounded-md" />
        </div>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="border-border bg-card rounded-xl border p-4">
            <Skeleton className="mb-3 h-3 w-20" />
            <Skeleton className="h-7 w-14" />
            <Skeleton className="mt-2 h-3 w-28" />
          </div>
        ))}
      </div>

      {/* Content cards */}
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="border-border bg-card space-y-3 rounded-xl border p-4">
            <div className="flex items-center gap-3">
              <Skeleton className="size-8 rounded-full" />
              <div className="flex-1 space-y-1.5">
                <Skeleton className="h-4 w-44" />
                <Skeleton className="h-3 w-28" />
              </div>
              <Skeleton className="h-6 w-16 rounded-full" />
            </div>
            <Skeleton className="h-1.5 w-full rounded-full" />
            <div className="flex gap-2">
              {Array.from({ length: 4 }).map((_, j) => (
                <Skeleton key={j} className="h-3 w-12" />
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}
