/**
 * Web Vitals reporter — collects Core Web Vitals + custom timings on
 * the client and forwards them to:
 *
 *   1. The Sentry DSN when configured (via the global `Sentry` shim
 *      that the lazy Sentry init layer registers).
 *   2. A simple beacon to /api/vitals (server route) which the gateway
 *      Prometheus scraper picks up.
 *
 * Falls back to a no-op when neither sink is configured. Safe to call
 * on every page — the underlying `web-vitals` library only emits each
 * metric once per page lifecycle.
 */
import { onCLS, onINP, onLCP, onTTFB, onFCP, type Metric } from 'web-vitals'
import { env } from '@/lib/env'
import { captureException } from '@/lib/observability/sentry'

type Sink = (m: Metric) => void

const sinks: Sink[] = []

function consoleSink(m: Metric) {
  if (!env.debug) return
  // eslint-disable-next-line no-console
  console.warn(`[web-vitals] ${m.name}=${m.value.toFixed(1)} (${m.rating})`)
}

async function sentrySink(m: Metric) {
  if (!env.sentryDsn) return
  if (m.rating === 'poor') {
    await captureException(new Error(`web-vital-poor:${m.name}`), {
      name: m.name,
      value: m.value,
      rating: m.rating,
      id: m.id,
    })
  }
}

function beaconSink(m: Metric) {
  // The /api/vitals route lives in the same Next deployment so it
  // benefits from same-origin keepalive semantics. Fall back to fetch
  // when sendBeacon is unavailable (older Safari + WebViews).
  const body = JSON.stringify({
    name: m.name,
    value: m.value,
    rating: m.rating,
    id: m.id,
    delta: m.delta,
    navigationType: m.navigationType,
    release: env.release,
  })
  try {
    if (typeof navigator !== 'undefined' && 'sendBeacon' in navigator) {
      navigator.sendBeacon('/api/vitals', body)
      return
    }
  } catch {
    /* fall through to fetch */
  }
  try {
    void fetch('/api/vitals', {
      method: 'POST',
      keepalive: true,
      headers: { 'content-type': 'application/json' },
      body,
    })
  } catch {
    /* swallow — vitals must never break the page */
  }
}

let started = false

/** Register the listeners once. Idempotent: calling more than once is
 *  cheap and only attaches the underlying observers a single time. */
export function startWebVitals(): void {
  if (started || typeof window === 'undefined') return
  started = true
  sinks.push(consoleSink, beaconSink)
  const dispatch = (m: Metric) => {
    sinks.forEach((s) => s(m))
    void sentrySink(m)
  }
  onCLS(dispatch)
  onINP(dispatch)
  onLCP(dispatch)
  onTTFB(dispatch)
  onFCP(dispatch)
}
