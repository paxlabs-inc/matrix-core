'use client'

import { Link } from '@/i18n/navigation'
import { Button } from '@/components/ui/button'

export default function AuthCallbackError({ reset }: { reset: () => void }) {
  return (
    <main className="bg-background flex min-h-dvh flex-col items-center justify-center gap-4 p-6 text-center">
      <h1 className="text-lg font-semibold">Sign-in failed</h1>
      <p className="text-muted-foreground max-w-sm text-sm">
        We could not complete authentication. Your link may have expired.
      </p>
      <div className="flex gap-2">
        <Button variant="outline" asChild>
          <Link href="/login">Back to sign in</Link>
        </Button>
        <Button onClick={reset}>Try again</Button>
      </div>
    </main>
  )
}
