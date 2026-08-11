'use client'

/**
 * Auth callback — implicit-flow return target for magic-link and OAuth.
 * See inline comments in the original implementation.
 */
import { Suspense, useEffect, useState } from 'react'
import { useSearchParams } from 'next/navigation'
import { useRouter } from '@/i18n/navigation'
import { useAuth } from '@/lib/auth/AuthProvider'
import { stripLocalePrefixFromHref } from '@/lib/i18n/proxy-path'
import { CentraLogo } from '@/components/brand/centra-logo'

function safeNext(next: string | null): string {
  if (!next) return '/'
  if (!next.startsWith('/') || next.startsWith('//')) return '/'
  // The i18n router re-prefixes the active locale on navigate; a locale-
  // prefixed `next` would land on /en/en/... → 404. Strip it defensively
  // (older deployed links may still carry the prefixed form).
  return stripLocalePrefixFromHref(next)
}

function hashError(): string | null {
  if (typeof window === 'undefined') return null
  const h = window.location.hash.startsWith('#') ? window.location.hash.slice(1) : ''
  if (!h) return null
  const p = new URLSearchParams(h)
  const desc = p.get('error_description') || p.get('error')
  return desc ? desc.replace(/\+/g, ' ') : null
}

function AuthCallbackInner() {
  const router = useRouter()
  const params = useSearchParams()
  const { session, loading, enabled } = useAuth()
  const next = safeNext(params.get('next'))

  const [error, setError] = useState<string | null>(null)
  const [timedOut, setTimedOut] = useState(false)

  useEffect(() => {
    const e = hashError()
    if (e) setError(e)
  }, [])

  useEffect(() => {
    if (session) router.replace(next)
  }, [session, next, router])

  useEffect(() => {
    if (!enabled) setError('Sign-in is not configured.')
  }, [enabled])

  useEffect(() => {
    const id = setTimeout(() => setTimedOut(true), 15_000)
    return () => clearTimeout(id)
  }, [])

  const failed = error || (timedOut && !session)

  return (
    <main className="bg-background flex min-h-dvh flex-col items-center justify-center gap-6 px-6">
      <CentraLogo size="lg" />
      {failed ? (
        <div className="flex max-w-sm flex-col items-center gap-4 text-center">
          <p className="text-foreground text-sm font-medium">Couldn’t complete sign-in</p>
          <p className="text-muted-foreground text-xs text-pretty">
            {error ?? 'Your sign-in link may have expired. Please request a new one.'}
          </p>
          <button
            type="button"
            onClick={() => router.replace(`/login?next=${encodeURIComponent(next)}`)}
            className="border-border hover:bg-accent rounded-md border px-4 py-2 text-sm transition"
          >
            Back to sign in
          </button>
        </div>
      ) : (
        <div className="flex flex-col items-center gap-3">
          <div
            className="border-muted-foreground/30 border-t-primary size-6 animate-spin rounded-full border-2"
            aria-hidden
          />
          <p className="text-muted-foreground text-sm">
            {loading ? 'Signing you in…' : 'Finishing up…'}
          </p>
        </div>
      )}
    </main>
  )
}

export default function AuthCallbackPage() {
  return (
    <Suspense fallback={null}>
      <AuthCallbackInner />
    </Suspense>
  )
}
