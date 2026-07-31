/**
 * Browser-side Supabase client. Lazily initialised so we don't crash
 * the bundle when Supabase env is unset (dev mode without auth).
 *
 * The client is constructed once per browser tab and then reused — the
 * library keeps its own auth state machine (refresh timer, listener
 * registry) on the singleton.
 */
import { createBrowserClient } from '@supabase/ssr'
import type { SupabaseClient } from '@supabase/supabase-js'
import { env, authEnabled } from '@/lib/env'

let _client: SupabaseClient | null = null

export function getSupabase(): SupabaseClient | null {
  if (!authEnabled) return null
  if (_client) return _client
  if (typeof window === 'undefined') {
    return null
  }
  _client = createBrowserClient(env.supabaseUrl as string, env.supabaseAnonKey as string, {
    global: {
      headers: { 'x-matrix-client': `next/${env.release}` },
    },
    auth: {
      detectSessionInUrl: true,
      // Implicit flow: magic-link / OAuth tokens arrive in the URL hash so
      // sign-in works when the link opens in a different webview than the
      // one that started the flow. After setSession, cookies are mirrored
      // to httpOnly storage via /api/auth/session.
      flowType: 'implicit',
    },
  })
  return _client
}
