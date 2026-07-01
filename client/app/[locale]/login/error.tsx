'use client'

import { Link } from '@/i18n/navigation'
import { Button } from '@/components/ui/button'

export default function LoginError({ reset }: { reset: () => void }) {
  return (
    <main className="bg-background flex min-h-screen flex-col items-center justify-center gap-4 p-6 text-center">
      <h1 className="text-lg font-semibold">Sign-in unavailable</h1>
      <p className="text-muted-foreground max-w-sm text-sm">
        Something went wrong loading the sign-in page. Try again or return home.
      </p>
      <div className="flex gap-2">
        <Button variant="outline" asChild>
          <Link href="/">Home</Link>
        </Button>
        <Button onClick={reset}>Try again</Button>
      </div>
    </main>
  )
}
