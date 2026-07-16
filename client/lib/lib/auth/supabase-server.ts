/**
 * Server-side Supabase client for the edge proxy and route handlers.
 * Persists sessions in httpOnly Secure cookies when deployed over HTTPS.
 */
import { createServerClient } from '@supabase/ssr'
import type { NextRequest, NextResponse } from 'next/server'
import { authEnabled, env } from '@/lib/env'

export function createSupabaseServerClient(request: NextRequest, response: NextResponse) {
  if (!authEnabled) return null
  return createServerClient(env.supabaseUrl as string, env.supabaseAnonKey as string, {
    cookies: {
      getAll() {
        return request.cookies.getAll()
      },
      setAll(cookiesToSet) {
        for (const { name, value, options } of cookiesToSet) {
          response.cookies.set(name, value, options)
        }
      },
    },
  })
}
