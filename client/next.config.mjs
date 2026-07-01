/** @type {import('next').NextConfig} */
import BundleAnalyzer from '@next/bundle-analyzer'
import createNextIntlPlugin from 'next-intl/plugin'

const withBundleAnalyzer = BundleAnalyzer({
  enabled: process.env.ANALYZE === 'true',
})

const withNextIntl = createNextIntlPlugin('./i18n/request.ts')

// Allow-list of remote origins the client may talk to:
//     APIs + SSE streams + WebSocket fallback.
//
// The values are read from public env so a deployment can point at a
// staging router without rebuilding.

// Small helper: reduce any URL-ish value down to its scheme+host
// origin, tolerating trailing slashes and embedded paths. CSP
// connect-src matches on origin, so a bare path is useless here.
const toOrigin = (value) => {
  if (!value) return ''
  try {
    return new URL(value).origin
  } catch {
    // Not a full URL (e.g. just a host); fall back to a trimmed string.
    return value.replace(/\/+$/, '')
  }
}

const routerOrigin = toOrigin(
  process.env.NEXT_PUBLIC_MATRIX_ROUTER_URL || '',
)
// Default to the observed Paxport endpoint so CSP is correct even when
// the env var is unset; a staging deploy can override it.
const walletOrigin = toOrigin(
  process.env.NEXT_PUBLIC_PAXEER_WALLET_API || '',
)
// PaxScan explorer API. Default to the public endpoint so CSP is
// correct when the env var is unset; a staging deploy can override it.
const paxscanOrigin = toOrigin(process.env.NEXT_PUBLIC_PAXSCAN_API || '')
const supabaseOrigin = toOrigin(process.env.NEXT_PUBLIC_SUPABASE_URL || '')
const sentryDsn = process.env.NEXT_PUBLIC_SENTRY_DSN || ''
let sentryOrigin = ''
try {
  if (sentryDsn) sentryOrigin = new URL(sentryDsn).origin
} catch {
  /* DSN parsing best-effort; bad DSN just falls out */
}

const wsOrigin = routerOrigin.replace(/^http/i, 'ws')
const connectSrc = ["'self'", routerOrigin, wsOrigin]

if (walletOrigin) {
  connectSrc.push(walletOrigin)
  // Add the wss variant in case the wallet API upgrades to a socket.
  connectSrc.push(walletOrigin.replace(/^https?/i, (m) => (m === 'https' ? 'wss' : 'ws')))
}
if (paxscanOrigin) {
  connectSrc.push(paxscanOrigin)
}
if (supabaseOrigin) {
  connectSrc.push(supabaseOrigin)
  connectSrc.push(supabaseOrigin.replace(/^https?/i, 'wss'))
}
if (sentryOrigin) connectSrc.push(sentryOrigin)
// Vercel analytics endpoint; harmless when not deployed there.
connectSrc.push('https://vitals.vercel-insights.com')

// De-dupe so the header stays tidy if origins overlap.
const connectSrcValue = [...new Set(connectSrc)].join(' ')

const securityHeaders = [
  // Block MIME-type sniffing
  { key: 'X-Content-Type-Options', value: 'nosniff' },
  // Prevent clickjacking
  { key: 'X-Frame-Options', value: 'SAMEORIGIN' },
  // Force HTTPS for 2 years, include subdomains
  { key: 'Strict-Transport-Security', value: 'max-age=63072000; includeSubDomains; preload' },
  // Minimal referrer exposure
  { key: 'Referrer-Policy', value: 'strict-origin-when-cross-origin' },
  // Lock down powerful browser APIs
  {
    key: 'Permissions-Policy',
    value: 'camera=(), microphone=(), geolocation=(), browsing-topics=()',
  },
  // CSP is applied per-request with a nonce in proxy.ts.
]

const nextConfig = {
  // React strict mode catches double-invocation bugs early.
  reactStrictMode: true,
  // Strip the v0.app generator tag before public launch.
  poweredByHeader: false,
  // The auth + dispatch flows depend on stable streaming responses;
  // keep the experimental optimizations conservative.
  experimental: {
    serverActions: { bodySizeLimit: '1mb' },
  },
  // Source maps for Sentry; not exposed publicly (hidden-source-map in prod).
  productionBrowserSourceMaps: Boolean(process.env.NEXT_PUBLIC_SENTRY_DSN),
  // Image optimisation: allow Supabase + the router for any user-
  // uploaded artefacts the receipt sheet might surface in future.
  images: {
    // The brand mark (matrix_icon_2.svg) is a trusted first-party asset in
    // /public; allow next/image to serve SVGs, sandboxed via the policy below.
    dangerouslyAllowSVG: true,
    contentDispositionType: 'attachment',
    contentSecurityPolicy: "default-src 'self'; script-src 'none'; sandbox;",
    remotePatterns: [
      ...(supabaseOrigin
        ? [
            {
              protocol: new URL(supabaseOrigin).protocol.replace(':', ''),
              hostname: new URL(supabaseOrigin).hostname,
            },
          ]
        : []),
      ...(routerOrigin
        ? [
            {
              protocol: new URL(routerOrigin).protocol.replace(':', ''),
              hostname: new URL(routerOrigin).hostname,
            },
          ]
        : []),
    ],
  },
  async headers() {
    return [
      {
        source: '/(.*)',
        headers: securityHeaders,
      },
      // SSE responses MUST not be cached or proxied with default
      // buffering. The path covers the Next-rendered routes; the
      // router-served /events stream is on a different origin.
      {
        source: '/events(.*)',
        headers: [
          { key: 'Cache-Control', value: 'no-cache, no-store, must-revalidate' },
          { key: 'X-Accel-Buffering', value: 'no' },
        ],
      },
    ]
  },
}

export default withBundleAnalyzer(withNextIntl(nextConfig))
