// Shared networking + result helpers for the paxeer-net bridge.

import { LIMITS } from './config.mjs'

// Shape a value into an MCP tool result. Objects are JSON-encoded.
export function ok(obj) {
  return { content: [{ type: 'text', text: typeof obj === 'string' ? obj : JSON.stringify(obj) }] }
}

// One HTTP round-trip returning parsed JSON (or raw text for non-JSON 200s).
// Throws an Error with `.status` / `.body` on a non-2xx response.
export async function httpJson(method, url, { headers = {}, body, timeoutMs = LIMITS.httpTimeoutMs } = {}) {
  if (typeof fetch !== 'function') throw new Error('paxeer: global fetch unavailable (Node 18+ required)')
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), Math.max(1000, timeoutMs))
  let res
  try {
    res = await fetch(url, {
      method,
      headers: {
        Accept: 'application/json',
        ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
        ...headers,
      },
      body: body !== undefined ? (typeof body === 'string' ? body : JSON.stringify(body)) : undefined,
      signal: controller.signal,
    })
  } catch (e) {
    clearTimeout(timer)
    const reason = e && e.name === 'AbortError' ? `timed out after ${timeoutMs}ms` : (e && e.message) || String(e)
    throw new Error(`paxeer: ${method} ${url} failed: ${reason}`)
  }
  clearTimeout(timer)
  let raw = await res.text()
  if (raw.length > LIMITS.maxResponseBytes) raw = raw.slice(0, LIMITS.maxResponseBytes)
  let parsed = null
  try {
    parsed = raw ? JSON.parse(raw) : null
  } catch {
    /* non-JSON body */
  }
  if (!res.ok) {
    const m = parsed && (parsed.message || parsed.error) ? parsed.message || parsed.error : raw.slice(0, 500)
    const e = new Error(`HTTP ${res.status} from ${url}: ${typeof m === 'string' ? m : JSON.stringify(m)}`)
    e.status = res.status
    e.body = parsed ?? raw
    throw e
  }
  return parsed === null && raw ? raw : parsed
}

export const httpGet = (url, opts) => httpJson('GET', url, opts)
export const httpPost = (url, body, opts) => httpJson('POST', url, { ...opts, body })

// Build a query string from a params object (skips undefined/null).
export function qs(params) {
  if (!params) return ''
  const s = Object.entries(params)
    .filter(([, v]) => v !== undefined && v !== null && v !== '')
    .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`)
    .join('&')
  return s ? `?${s}` : ''
}

// Decimal/whole-units -> integer base units (string). e.g. "1.5", 18 -> "1500000000000000000".
export function toBaseUnits(amount, decimals) {
  const s = String(amount).trim()
  const neg = s.startsWith('-')
  const a = neg ? s.slice(1) : s
  const [whole, frac = ''] = a.split('.')
  if (!/^\d*$/.test(whole) || !/^\d*$/.test(frac)) throw new Error(`paxeer: invalid amount "${amount}"`)
  const fracPad = (frac + '0'.repeat(decimals)).slice(0, decimals)
  const v = BigInt(whole || '0') * 10n ** BigInt(decimals) + BigInt(fracPad || '0')
  return (neg ? -v : v).toString()
}

// Integer base units -> decimal string. e.g. "1500000000000000000", 18 -> "1.5".
export function fromBaseUnits(base, decimals) {
  let v = BigInt(base)
  const neg = v < 0n
  if (neg) v = -v
  const d = 10n ** BigInt(decimals)
  const whole = v / d
  const fracStr = (v % d).toString().padStart(decimals, '0').replace(/0+$/, '')
  return (neg ? '-' : '') + whole.toString() + (fracStr ? '.' + fracStr : '')
}
