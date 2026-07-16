/**
 * Lazy Sentry initialisation — no-ops when NEXT_PUBLIC_SENTRY_DSN is unset
 * so local dev and CI builds stay dependency-light.
 */
import { env } from '@/lib/env'

let initPromise: Promise<void> | null = null

export function initSentryClient(): Promise<void> {
  if (typeof window === 'undefined') return Promise.resolve()
  if (!env.sentryDsn) return Promise.resolve()
  if (initPromise) return initPromise
  initPromise = import('@sentry/nextjs').then((Sentry) => {
    Sentry.init({
      dsn: env.sentryDsn,
      release: env.release,
      environment: process.env.NEXT_PUBLIC_VERCEL_ENV ?? process.env.NODE_ENV,
      tracesSampleRate: 0.1,
      replaysSessionSampleRate: 0,
      replaysOnErrorSampleRate: 0,
    })
  })
  return initPromise
}

export async function captureException(error: unknown, context?: Record<string, unknown>) {
  if (!env.sentryDsn) return
  await initSentryClient()
  const Sentry = await import('@sentry/nextjs')
  Sentry.captureException(error, context ? { extra: context } : undefined)
}
