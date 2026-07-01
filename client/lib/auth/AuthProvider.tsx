'use client'

/**
 * AuthProvider — exposes the authenticated user (or null) to the React
 * tree and listens for Supabase auth changes so the app re-renders when
 * the user signs in/out or a refresh updates the session.
 *
 * The provider stays purely client-side; SSR returns `loading=true` so
 * the tree can render a stable placeholder before hydration.
 */
import { createContext, useContext, useEffect, useMemo, useState } from 'react'
import type { Session, User } from '@supabase/supabase-js'
import { authEnabled } from '@/lib/env'
import { getSession, onAuthChange, signOut as doSignOut } from '@/lib/auth/session'

export interface AuthState {
  /** Current session, or null when unauthenticated. */
  session: Session | null
  /** Convenience pointer at session.user. */
  user: User | null
  /** True until the first session probe resolves. */
  loading: boolean
  /** True when the runtime supports auth (Supabase env present). */
  enabled: boolean
  signOut: () => Promise<void>
}

const Ctx = createContext<AuthState | null>(null)

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [session, setSession] = useState<Session | null>(null)
  const [loading, setLoading] = useState<boolean>(true)

  useEffect(() => {
    let cancelled = false
    if (!authEnabled) {
      setLoading(false)
      return () => {}
    }
    getSession()
      .then((s) => {
        if (!cancelled) {
          setSession(s)
          syncSessionMarker(s)
          setLoading(false)
        }
      })
      .catch(() => {
        if (!cancelled) setLoading(false)
      })
    const off = onAuthChange((_evt, s) => {
      if (!cancelled) {
        setSession(s)
        syncSessionMarker(s)
      }
    })
    return () => {
      cancelled = true
      off()
    }
  }, [])

  const value = useMemo<AuthState>(
    () => ({
      session,
      user: session?.user ?? null,
      loading,
      enabled: authEnabled,
      signOut: doSignOut,
    }),
    [session, loading],
  )

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useAuth(): AuthState {
  const v = useContext(Ctx)
  if (!v) {
    throw new Error('useAuth must be used inside <AuthProvider>')
  }
  return v
}

/** Friendly display name for the signed-in user, derived from OAuth
 *  profile metadata (Google/GitHub) or the email local-part. Returns
 *  null when nothing human-friendly is available so callers fall back
 *  to a neutral greeting rather than surfacing a raw id or agent DID. */
export function userDisplayName(user: User | null): string | null {
  if (!user) return null
  const meta = (user.user_metadata ?? {}) as Record<string, unknown>
  for (const key of ['full_name', 'name', 'user_name', 'preferred_username']) {
    const v = meta[key]
    if (typeof v === 'string' && v.trim()) return v.trim()
  }
  if (user.email) {
    const local = user.email.split('@')[0]
    if (local) return local
  }
  return null
}

/** Mirror session into httpOnly Supabase cookies (server) and a UX-only
 *  mx-session marker the edge proxy reads for redirects. */
function syncSessionMarker(session: Session | null): void {
  if (typeof document === 'undefined') return
  const name = 'mx-session'
  if (!session) {
    document.cookie = `${name}=; Path=/; Max-Age=0; SameSite=Lax; Secure`
    return
  }
  const exp = session.expires_at ?? Math.floor(Date.now() / 1000) + 3600
  const maxAge = Math.max(0, exp - Math.floor(Date.now() / 1000))
  const sub = session.user?.id ?? ''
  const secure = window.location.protocol === 'https:' ? '; Secure' : ''
  document.cookie = `${name}=${encodeURIComponent(sub)}; Path=/; Max-Age=${maxAge}; SameSite=Lax${secure}`

  void fetch('/api/auth/session', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify({
      access_token: session.access_token,
      refresh_token: session.refresh_token,
    }),
  }).catch(() => {
    /* non-fatal — client cookies still work for API calls */
  })
}
