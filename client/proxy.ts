import createIntlMiddleware from 'next-intl/middleware'
import { type NextRequest, NextResponse } from 'next/server'
import { checkRateLimit } from '@/lib/rate-limit'
import { createSupabaseServerClient } from '@/lib/auth/supabase-server'
import { authEnabled } from '@/lib/env'
import { buildContentSecurityPolicy } from '@/lib/security/csp'
import { routing, defaultLocale } from '@/i18n/routing'
import { localePrefix, stripLocalePrefix } from '@/lib/i18n/proxy-path'

const intlMiddleware = createIntlMiddleware(routing)

const PUBLIC_INNER_PATHS = new Set<string>([
  '/login',
  '/auth/callback',
  '/manifest.webmanifest',
  '/robots.txt',
  '/favicon.ico',
  '/privacy',
  '/terms',
  '/legal',
  '/acceptable-use',
  '/cookies',
  '/risk-disclosure',
  '/dmca',
])

const PUBLIC_API_PREFIXES = ['/api/vitals', '/api/auth/session']

function clientIp(req: NextRequest): string {
  const xff = req.headers.get('x-forwarded-for')
  if (xff) return xff.split(',')[0].trim()
  return req.headers.get('x-real-ip') ?? 'unknown'
}

function nonce(): string {
  const bytes = new Uint8Array(12)
  crypto.getRandomValues(bytes)
  let bin = ''
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i])
  if (typeof btoa === 'function') return btoa(bin).replace(/=+$/, '')
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}

function isPublicPath(pathname: string): boolean {
  if (PUBLIC_INNER_PATHS.has(pathname)) return true
  if (PUBLIC_API_PREFIXES.some((p) => pathname.startsWith(p))) return true
  if (pathname.startsWith('/_next')) return true
  // The static legal & compliance site (public/legal/*.html + its css/fonts)
  // must be reachable without a session — it's the public record.
  if (pathname.startsWith('/legal/')) return true
  return false
}

function hasSessionCookie(req: NextRequest): boolean {
  if ((req.cookies.get('mx-session')?.value ?? '').length > 0) return true
  for (const c of req.cookies.getAll()) {
    if (c.name.startsWith('sb-') && c.name.includes('auth-token') && c.value.length > 0) {
      return true
    }
  }
  return false
}

function decorate(res: NextResponse, cspNonce: string, rl: { remaining: number }): NextResponse {
  res.headers.set('x-csp-nonce', cspNonce)
  res.headers.set('Content-Security-Policy', buildContentSecurityPolicy(cspNonce))
  res.headers.set('X-RateLimit-Limit', '240')
  res.headers.set('X-RateLimit-Remaining', String(rl.remaining))
  return res
}

export async function proxy(request: NextRequest) {
  const ip = clientIp(request)
  const rl = await checkRateLimit(ip)
  if (!rl.ok) {
    const res = new NextResponse('rate limited', { status: 429 })
    res.headers.set('Retry-After', Math.ceil((rl.reset - Date.now()) / 1000).toString())
    res.headers.set('X-RateLimit-Limit', '240')
    res.headers.set('X-RateLimit-Remaining', '0')
    return res
  }

  const cspNonce = nonce()

  const requestHeaders = new Headers(request.headers)
  requestHeaders.set('x-csp-nonce', cspNonce)

  const { locale, pathname: innerPath } = stripLocalePrefix(request.nextUrl.pathname)
  const activeLocale = locale ?? defaultLocale

  // API routes and Next internals must bypass the next-intl middleware. With
  // localePrefix:'always', next-intl 307-redirects any path lacking a locale
  // prefix to add one — turning POST /api/auth/session into
  // /en/api/auth/session, which has no route handler (the route handlers live
  // OUTSIDE [locale]), so the request 404s. Rate-limiting, CSP and the auth
  // gate below still apply to these paths; only the locale rewrite is skipped.
  const bypassIntl =
    request.nextUrl.pathname.startsWith('/api') ||
    request.nextUrl.pathname.startsWith('/_next') ||
    // Static legal site assets live under /legal/ in /public — a locale
    // redirect would 404 them (there is no /en/legal/*.html file).
    request.nextUrl.pathname.startsWith('/legal/')

  let response = bypassIntl
    ? NextResponse.next({ request: { headers: requestHeaders } })
    : intlMiddleware(request)
  response.headers.set('x-csp-nonce', cspNonce)

  decorate(response, cspNonce, rl)

  if (authEnabled) {
    const supabase = createSupabaseServerClient(request, response)
    if (supabase) {
      await supabase.auth.getUser()
    }
  }

  if (!isPublicPath(innerPath)) {
    const hasSession = hasSessionCookie(request)
    const devEscape =
      process.env.NODE_ENV !== 'production' && request.nextUrl.searchParams.get('dev') === '1'
    if (!hasSession && !devEscape) {
      const url = request.nextUrl.clone()
      url.pathname = localePrefix('/login', activeLocale)
      // `next` must be the locale-STRIPPED inner path: the login/callback
      // pages navigate with the i18n router, which re-prefixes the active
      // locale. A prefixed value here would yield /en/en/... → 404.
      url.searchParams.set('next', innerPath + request.nextUrl.search)
      response = NextResponse.redirect(url)
      decorate(response, cspNonce, rl)
    }
  }

  return response
}

export const config = {
  matcher: ['/((?!_next/static|_next/image|favicon.ico|.*\\.(?:svg|png|jpg|jpeg|gif|webp)$).*)'],
}
