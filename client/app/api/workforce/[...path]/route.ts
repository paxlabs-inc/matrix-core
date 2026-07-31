import { NextResponse } from 'next/server'

export const runtime = 'nodejs'
export const dynamic = 'force-dynamic'

interface RouteContext {
  params: Promise<{ path: string[] }>
}

function localTarget(): URL | null {
  if (process.env.WORKFORCE_LOCAL_PROXY_ENABLED !== '1') return null
  const raw = process.env.WORKFORCE_LOCAL_URL?.trim()
  if (!raw) return null
  try {
    const target = new URL(raw)
    if (
      target.protocol !== 'http:' ||
      !['127.0.0.1', 'localhost', '::1'].includes(target.hostname)
    ) {
      return null
    }
    target.pathname = target.pathname.replace(/\/+$/, '')
    return target
  } catch {
    return null
  }
}

function sameOriginRequest(request: Request): boolean {
  const fetchSite = request.headers.get('sec-fetch-site')
  if (fetchSite && fetchSite !== 'same-origin') return false
  const origin = request.headers.get('origin')
  if (!origin) return true
  try {
    const originURL = new URL(origin)
    const requestHost = request.headers.get('host')
    return (
      originURL.protocol === 'http:' &&
      ['127.0.0.1', 'localhost', '::1'].includes(originURL.hostname) &&
      Boolean(requestHost && originURL.host === requestHost)
    )
  } catch {
    return false
  }
}

async function proxy(request: Request, context: RouteContext): Promise<Response> {
  const target = localTarget()
  const token = process.env.WORKFORCE_LOCAL_OWNER_TOKEN?.trim()
  if (!target || !token) {
    return NextResponse.json({ error: 'local Workforce proxy is disabled' }, { status: 404 })
  }
  if (!sameOriginRequest(request)) {
    return NextResponse.json(
      { error: 'cross-origin Workforce proxy request denied' },
      { status: 403 },
    )
  }
  const { path } = await context.params
  if (!path.length || path.some((segment) => !segment || segment === '.' || segment === '..')) {
    return NextResponse.json({ error: 'invalid Workforce proxy path' }, { status: 400 })
  }
  const incoming = new URL(request.url)
  const upstream = new URL(target)
  const targetPath = target.pathname.replace(/\/+$/, '')
  upstream.pathname = `${targetPath}/${path.map(encodeURIComponent).join('/')}`
  upstream.search = incoming.search
  const headers = new Headers()
  headers.set('accept', request.headers.get('accept') ?? 'application/json')
  headers.set('authorization', `Bearer ${token}`)
  const contentType = request.headers.get('content-type')
  if (contentType) headers.set('content-type', contentType)
  const body =
    request.method === 'GET' || request.method === 'HEAD' ? undefined : await request.arrayBuffer()
  try {
    const response = await fetch(upstream, {
      method: request.method,
      headers,
      body,
      cache: 'no-store',
      signal: request.signal,
    })
    const responseHeaders = new Headers()
    for (const name of ['cache-control', 'content-type']) {
      const value = response.headers.get(name)
      if (value) responseHeaders.set(name, value)
    }
    return new Response(response.body, {
      status: response.status,
      headers: responseHeaders,
    })
  } catch {
    return NextResponse.json({ error: 'local Workforce backend is unavailable' }, { status: 502 })
  }
}

export function GET(request: Request, context: RouteContext) {
  return proxy(request, context)
}

export function POST(request: Request, context: RouteContext) {
  return proxy(request, context)
}
