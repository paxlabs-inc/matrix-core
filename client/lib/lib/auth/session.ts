/**
 * Session helpers — wrap Supabase's auth API with single-flight token
 * refresh and a synchronous getter for the current access token. The
 * API client uses `getAccessToken()` once per request; the underlying
 * Supabase client refreshes in the background well before expiry.
 *
 * Falls back to anonymous behaviour (returning `null`) when auth is
 * not configured, so dev workflows that hit a non-authed local daemon
 * still work.
 */
import type { Session, AuthChangeEvent } from '@supabase/supabase-js'
import { getSupabase } from '@/lib/auth/supabase'

let _refresh: Promise<Session | null> | null = null

/** Returns the current Supabase session (cached by the supabase-js
 *  client) or `null` when unauthenticated / unconfigured. */
export async function getSession(): Promise<Session | null> {
  const sb = getSupabase()
  if (!sb) return null
  const { data, error } = await sb.auth.getSession()
  if (error) {
    if (typeof console !== 'undefined') console.warn('auth: getSession failed', error.message)
    return null
  }
  return data.session
}

/** Returns a valid access token, triggering a single-flight refresh if
 *  the cached session is expired. Returns `null` when unauthenticated. */
export async function getAccessToken(): Promise<string | null> {
  const sb = getSupabase()
  if (!sb) return null
  const session = await getSession()
  if (!session) return null

  // Refresh ~60s ahead of expiry so we don't race the clock during a
  // long-running SSE reconnect.
  const expSec = session.expires_at ?? 0
  const skewSec = 60
  const nowSec = Math.floor(Date.now() / 1000)
  if (expSec === 0 || expSec - nowSec > skewSec) {
    return session.access_token
  }

  if (!_refresh) {
    _refresh = sb.auth
      .refreshSession()
      .then(({ data }) => data.session ?? null)
      .finally(() => {
        // Drop the in-flight promise either way so the next call kicks
        // off a fresh refresh on the next near-expiry token.
        _refresh = null
      })
  }
  const refreshed = await _refresh
  return refreshed?.access_token ?? null
}

let _reauth: Promise<boolean> | null = null

/** Handle a server-side 401: force a single-flight token refresh. If a valid
 *  session is recovered the queries can simply refetch (resolves true);
 *  otherwise the browser is routed to the login page so the user re-auths
 *  instead of staring at a dead card. */
export async function handleUnauthorized(): Promise<boolean> {
  const sb = getSupabase()
  if (!sb) return false
  if (!_reauth) {
    _reauth = sb.auth
      .refreshSession()
      .then(({ data }) => {
        if (data.session) return true
        redirectToLogin()
        return false
      })
      .catch(() => {
        redirectToLogin()
        return false
      })
      .finally(() => {
        _reauth = null
      })
  }
  return _reauth
}

/** Send the browser to the login page. No-op on the server and when already
 *  on the login / OAuth-callback route (avoids a redirect loop). next-intl
 *  re-prefixes the active locale, so a bare `/login` is correct here. */
function redirectToLogin(): void {
  if (typeof window === 'undefined') return
  const { pathname } = window.location
  if (pathname.includes('/login') || pathname.includes('/auth/callback')) return
  window.location.assign('/login')
}

/** Subscribe to auth state changes. Returns an unsubscribe function. */
export function onAuthChange(
  cb: (evt: AuthChangeEvent, session: Session | null) => void,
): () => void {
  const sb = getSupabase()
  if (!sb) return () => {}
  const { data } = sb.auth.onAuthStateChange((evt, session) => cb(evt, session))
  return () => data.subscription.unsubscribe()
}

export async function signInWithEmail(email: string, redirectTo?: string): Promise<void> {
  const sb = getSupabase()
  if (!sb) throw new Error('auth: Supabase is not configured')
  const { error } = await sb.auth.signInWithOtp({
    email,
    options: { emailRedirectTo: redirectTo },
  })
  if (error) throw error
}

/** Provider OAuth sign-in (Google, GitHub). Kicks off the redirect
 *  flow; the browser navigates to the provider and returns to
 *  `redirectTo` where the PKCE code is exchanged for a session
 *  (detectSessionInUrl is enabled on the client). */
export async function signInWithOAuth(
  provider: 'google' | 'github',
  redirectTo?: string,
): Promise<void> {
  const sb = getSupabase()
  if (!sb) throw new Error('auth: Supabase is not configured')
  const { error } = await sb.auth.signInWithOAuth({
    provider,
    options: redirectTo ? { redirectTo } : undefined,
  })
  if (error) throw error
}

export async function signOut(): Promise<void> {
  const sb = getSupabase()
  if (!sb) return
  await sb.auth.signOut()
}
