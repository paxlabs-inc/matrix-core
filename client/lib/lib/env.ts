/**
 * Environment configuration — single source of truth for every runtime
 * value the client reads. Validation happens at module load so a missing
 * value fails fast with a clear message instead of silently returning
 * `undefined` deep inside a fetch call.
 *
 * Public (NEXT_PUBLIC_*) values are inlined into the client bundle by
 * Next.js. Anything sensitive (Supabase service role, gateway tokens)
 * MUST stay out of this file — they belong on the router/gateway only.
 */
import { z } from 'zod'

const PublicSchema = z.object({
  /** Base URL of the Matrix Router (the public-facing JWT-protected proxy). */
  routerUrl: z.string().url().default('https://api.paxlabs.app'),
  /** Supabase project URL — used by the browser client for auth. */
  supabaseUrl: z.string().url().optional(),
  /** Supabase anon (publishable) key. Safe to ship in the client bundle. */
  supabaseAnonKey: z.string().min(20).optional(),
  /** Sentry DSN; empty disables error tracking. */
  sentryDsn: z.string().optional(),
  /** Build identity used by RUM + Sentry release tagging. */
  release: z.string().default('dev'),
  /** Enables verbose console logging (dev only). */
  debug: z.boolean().default(false),
})

export type PublicEnv = z.infer<typeof PublicSchema>

function loadPublic(): PublicEnv {
  const parsed = PublicSchema.safeParse({
    routerUrl: process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL,
    supabaseUrl: process.env.NEXT_PUBLIC_SUPABASE_URL,
    supabaseAnonKey: process.env.NEXT_PUBLIC_SUPABASE_ANON_KEY,
    sentryDsn: process.env.NEXT_PUBLIC_SENTRY_DSN,
    release: process.env.NEXT_PUBLIC_RELEASE ?? process.env.NEXT_PUBLIC_VERCEL_GIT_COMMIT_SHA,
    debug: process.env.NEXT_PUBLIC_DEBUG === 'true',
  })
  if (!parsed.success) {
    // Throwing in module init halts the bundle from running with bad
    // config — the user sees a stack instead of mysterious 401s later.
    const issues = parsed.error.issues.map((i) => `${i.path.join('.')}: ${i.message}`).join('; ')
    throw new Error(`env: invalid public configuration: ${issues}`)
  }
  return parsed.data
}

export const env: PublicEnv = loadPublic()

/** True when Supabase auth is configured. Many code paths fall back to
 *  an unauthenticated dev mode when this is false so local development
 *  works without a backend. */
export const authEnabled: boolean = Boolean(env.supabaseUrl && env.supabaseAnonKey)

/** Trim trailing slash so URL composition stays predictable. */
export function routerOrigin(): string {
  return env.routerUrl.replace(/\/+$/, '')
}
