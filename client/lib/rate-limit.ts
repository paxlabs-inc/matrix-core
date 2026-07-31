/**
 * Edge rate limiter — Upstash Redis when configured, in-memory fallback
 * for local dev and single-instance deploys.
 */
import { Ratelimit } from '@upstash/ratelimit'
import { Redis } from '@upstash/redis'

const WINDOW_MS = 60_000
const LIMIT = 240

interface Bucket {
  count: number
  reset: number
}

const memory = new Map<string, Bucket>()

function memoryLimit(ip: string): { ok: boolean; remaining: number; reset: number } {
  const now = Date.now()
  const b = memory.get(ip)
  if (!b || b.reset < now) {
    memory.set(ip, { count: 1, reset: now + WINDOW_MS })
    return { ok: true, remaining: LIMIT - 1, reset: now + WINDOW_MS }
  }
  b.count++
  return {
    ok: b.count <= LIMIT,
    remaining: Math.max(0, LIMIT - b.count),
    reset: b.reset,
  }
}

let upstash: Ratelimit | null = null

function getUpstash(): Ratelimit | null {
  if (upstash) return upstash
  const url = process.env.UPSTASH_REDIS_REST_URL
  const token = process.env.UPSTASH_REDIS_REST_TOKEN
  if (!url || !token) return null
  upstash = new Ratelimit({
    redis: new Redis({ url, token }),
    limiter: Ratelimit.slidingWindow(LIMIT, '1 m'),
    prefix: 'matrix:rl',
  })
  return upstash
}

export async function checkRateLimit(
  ip: string,
): Promise<{ ok: boolean; remaining: number; reset: number }> {
  const remote = getUpstash()
  if (!remote) return memoryLimit(ip)
  const res = await remote.limit(ip)
  return {
    ok: res.success,
    remaining: res.remaining,
    reset: res.reset,
  }
}
