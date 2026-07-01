import { Skeleton } from '@/components/ui/skeleton'

export default function LoginLoading() {
  return (
    <main className="bg-background flex min-h-screen items-center justify-center p-6">
      <div className="flex w-full max-w-sm flex-col gap-4">
        <Skeleton className="mx-auto h-10 w-32" />
        <Skeleton className="h-10 w-full" />
        <Skeleton className="h-10 w-full" />
      </div>
    </main>
  )
}
