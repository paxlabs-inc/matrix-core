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

export interface PublicEnv {
  /** Base URL of the Centra AI Router (the public-facing JWT-protected proxy). */
  routerUrl: string
  /** Optional same-origin or absolute Workforce API base. Local development
   * uses `/api/workforce` so owner credentials remain server-side. */
  workforceApiBase?: string
  /** Supabase project URL — used by the browser client for auth. */
  supabaseUrl?: string
  /** Supabase anon (publishable) key. Safe to ship in the client bundle. */
  supabaseAnonKey?: string
  /** Sentry DSN; empty disables error tracking. */
  sentryDsn?: string
  /** Build identity used by RUM + Sentry release tagging. */
  release: string
  /** Enables verbose console logging (dev only). */
  debug: boolean
}

function validAbsoluteUrl(value: string): boolean {
  return URL.canParse(value)
}

function loadPublic(): PublicEnv {
  const routerUrl = process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL ?? 'https://api.paxlabs.app'
  const workforceApiBase = process.env.NEXT_PUBLIC_WORKFORCE_API_BASE
  const supabaseUrl = process.env.NEXT_PUBLIC_SUPABASE_URL
  const supabaseAnonKey = process.env.NEXT_PUBLIC_SUPABASE_ANON_KEY
  const issues: string[] = []

  if (!validAbsoluteUrl(routerUrl)) issues.push('routerUrl: invalid URL')
  if (
    workforceApiBase !== undefined &&
    !workforceApiBase.startsWith('/') &&
    !validAbsoluteUrl(workforceApiBase)
  ) {
    issues.push('workforceApiBase: must be a path or URL')
  }
  if (supabaseUrl !== undefined && !validAbsoluteUrl(supabaseUrl)) {
    issues.push('supabaseUrl: invalid URL')
  }
  if (supabaseAnonKey !== undefined && supabaseAnonKey.length < 20) {
    issues.push('supabaseAnonKey: must contain at least 20 characters')
  }
  if (issues.length > 0) {
    // Throwing in module init halts the bundle from running with bad
    // config — the user sees a stack instead of mysterious 401s later.
    throw new Error(`env: invalid public configuration: ${issues.join('; ')}`)
  }

  return {
    routerUrl,
    workforceApiBase,
    supabaseUrl,
    supabaseAnonKey,
    sentryDsn: process.env.NEXT_PUBLIC_SENTRY_DSN,
    release:
      process.env.NEXT_PUBLIC_RELEASE ?? process.env.NEXT_PUBLIC_VERCEL_GIT_COMMIT_SHA ?? 'dev',
    debug: process.env.NEXT_PUBLIC_DEBUG === 'true',
  }
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

export function workforceEndpoint(path: string): string {
  const suffix = path.startsWith('/') ? path : `/${path}`
  const base = env.workforceApiBase?.replace(/\/+$/, '')
  return base ? `${base}${suffix}` : `${routerOrigin()}${suffix}`
}
