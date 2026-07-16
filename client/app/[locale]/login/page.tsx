import { Suspense } from 'react'
import Image from 'next/image'
import LoginForm from './login-form'
import { MatrixLogo } from '@/components/matrix/matrix-logo'

export const dynamic = 'force-dynamic'
export const runtime = 'nodejs'

export const metadata = {
  title: 'Sign in',
}

export default function LoginPage() {
  return (
    <main className="bg-background grid min-h-screen grid-cols-1 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]">
      {/* Left: brand + auth. Layers separate by background tone — no strokes. */}
      <div className="relative flex flex-col px-6 py-8 sm:px-10 lg:px-16">
        <div className="flex items-center">
          <MatrixLogo size="md" />
        </div>

        <div className="mx-auto flex w-full max-w-sm flex-1 flex-col justify-center gap-8 py-10">
          <div className="flex flex-col gap-3">
            <h1 className="text-4xl font-semibold tracking-tight sm:text-5xl">
              Think fast,
              <br />
              build faster
            </h1>
            <p className="text-muted-foreground text-base"></p>
          </div>

          <Suspense fallback={null}>
            <LoginForm />
          </Suspense>
        </div>

        <p className="text-muted-foreground text-xs">&copy; {new Date().getFullYear()} Matrix</p>
      </div>

      {/* Right: full-bleed hero image, rounded and inset. */}
      <div className="hidden p-3 lg:block">
        <div className="bg-surface-secondary relative h-full w-full overflow-hidden rounded-3xl">
          <Image src="/Welcome_s.png" alt="" fill priority sizes="50vw" className="object-cover" />
        </div>
      </div>
    </main>
  )
}
