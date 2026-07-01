const toOrigin = (value: string | undefined): string => {
  if (!value) return ''
  try {
    return new URL(value).origin
  } catch {
    return value.replace(/\/+$/, '')
  }
}

/** Build connect-src allow-list for CSP (shared by proxy). */
export function buildConnectSrc(): string {
  const origins = new Set<string>(["'self'"])

  const router = toOrigin(process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL || 'https://matrix.paxeer.app')
  if (router) {
    origins.add(router)
    origins.add(router.replace(/^http/i, 'ws'))
  }

  for (const raw of [
    process.env.NEXT_PUBLIC_PAXEER_WALLET_API || 'https://connect.paxportwallet.com',
    process.env.NEXT_PUBLIC_PAXSCAN_API || 'https://api.paxscan.io',
    process.env.NEXT_PUBLIC_SUPABASE_URL,
  ]) {
    const o = toOrigin(raw)
    if (o) {
      origins.add(o)
      origins.add(o.replace(/^https?/i, (m) => (m === 'https' ? 'wss' : 'ws')))
    }
  }

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
    "frame-ancestors 'none'",
    "base-uri 'self'",
    "object-src 'none'",
    "form-action 'self'",
    "worker-src 'self' blob:",
  ].join('; ')
}
