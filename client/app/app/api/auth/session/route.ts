import { createServerClient } from '@supabase/ssr'
import { cookies } from 'next/headers'
import { NextResponse } from 'next/server'
import { authEnabled, env } from '@/lib/env'

export const runtime = 'edge'
export const dynamic = 'force-dynamic'

/**
 * POST /api/auth/session — persist Supabase tokens into httpOnly cookies
 * after the implicit-flow callback parses hash tokens on the client.
 */
export async function POST(request: Request) {
  if (!authEnabled) {
    return NextResponse.json({ error: 'auth disabled' }, { status: 503 })
  }

  let body: { access_token?: string; refresh_token?: string }
  try {
    body = await request.json()
  } catch {
    return NextResponse.json({ error: 'invalid json' }, { status: 400 })
  }

  const access_token = body.access_token?.trim()
  const refresh_token = body.refresh_token?.trim()
  if (!access_token || !refresh_token) {
    return NextResponse.json({ error: 'missing tokens' }, { status: 400 })
  }

  const cookieStore = await cookies()
  const supabase = createServerClient(env.supabaseUrl as string, env.supabaseAnonKey as string, {
    cookies: {
      getAll() {
        return cookieStore.getAll()
      },
      setAll(cookiesToSet) {
        for (const { name, value, options } of cookiesToSet) {
          cookieStore.set(name, value, options)
        }
      },
    },
  })

  const { error } = await supabase.auth.setSession({ access_token, refresh_token })
  if (error) {
    return NextResponse.json({ error: error.message }, { status: 401 })
  }

  return NextResponse.json({ ok: true })
}
