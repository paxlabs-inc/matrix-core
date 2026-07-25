const toOrigin = (value: string | undefined): string => {
  if (!value) return ''
  try {
    return new URL(value).origin
  } catch {
    return value.replace(/\/+$/, '')
  }
}

/** The Matrix Router origin the client talks to (API + SSE + preview proxy). */
export function routerOrigin(): string {
  return toOrigin(process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL || 'https://api.paxlabs.app')
}

function addHttpAndWebSocketOrigins(origins: Set<string>, origin: string): void {
  if (!origin) return
  origins.add(origin)
  if (/^https?:/i.test(origin)) {
    origins.add(origin.replace(/^http/i, 'ws'))
  } else if (/^wss?:/i.test(origin)) {
    origins.add(origin.replace(/^ws/i, 'http'))
  }
}

/**
 * Build frame-src allow-list. The Cody preview pane embeds the running app in
 * an <iframe> served by the router's /preview/{user} proxy, so the router
 * origin must be framable; without this the iframe falls back to default-src
 * 'self' and the preview is blocked.
 */
export function buildFrameSrc(): string {
  const origins = new Set<string>(["'self'"])
  const router = routerOrigin()
  if (router) origins.add(router)
  return [...origins].join(' ')
}

/** Build connect-src allow-list for CSP (shared by proxy). */
export function buildConnectSrc(): string {
  const origins = new Set<string>(["'self'"])

  const router = routerOrigin()
  addHttpAndWebSocketOrigins(origins, router)

  for (const raw of [
    process.env.NEXT_PUBLIC_PAXEER_WALLET_API || 'https://connect.paxportwallet.com',
    process.env.NEXT_PUBLIC_PAXSCAN_API || 'https://api.paxscan.io',
    process.env.NEXT_PUBLIC_SUPABASE_URL,
  ]) {
    const o = toOrigin(raw)
    addHttpAndWebSocketOrigins(origins, o)
  }

  const livekit = toOrigin(
    process.env.NEXT_PUBLIC_LIVEKIT_URL ||
      process.env.MATRIX_LIVEKIT_URL ||
      process.env.LIVEKIT_URL ||
      'wss://matrix-c94fehr4.livekit.cloud',
  )
  addHttpAndWebSocketOrigins(origins, livekit)

  const dsn = process.env.NEXT_PUBLIC_SENTRY_DSN
  if (dsn) {
    try {
      origins.add(new URL(dsn).origin)
    } catch {
      /* ignore */
    }
  }

  origins.add('https://vitals.vercel-insights.com')
  return [...origins].join(' ')
}

export function buildContentSecurityPolicy(nonceValue: string): string {
  const isDev = process.env.NODE_ENV === 'development'
  const scriptSrc = isDev
    ? `script-src 'self' 'nonce-${nonceValue}' 'unsafe-eval'`
    : `script-src 'self' 'nonce-${nonceValue}' 'strict-dynamic'`
  // style-src MUST NOT include a nonce: per CSP, when a nonce/hash is present
  // 'unsafe-inline' is ignored and React/Radix/motion inline style={} breaks.
  // Scripts stay nonce-locked; styles use the usual React-compatible policy.
  return [
    "default-src 'self'",
    scriptSrc,
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data: blob: https:",
    "font-src 'self' data:",
    `connect-src ${buildConnectSrc()}`,
    `frame-src ${buildFrameSrc()}`,
    "frame-ancestors 'none'",
    "base-uri 'self'",
    "object-src 'none'",
    "form-action 'self'",
    "worker-src 'self' blob:",
  ].join('; ')
}
