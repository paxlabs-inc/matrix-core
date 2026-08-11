/**
 * Centra AI HTTP client.
 *
 * Single fetch wrapper used by every resource module. Responsibilities:
 *
 *   - Compose the router origin + path
 *   - Attach the Supabase access token as Bearer
 *   - Stamp `x-request-id` for distributed tracing
 *   - Bound every call with an AbortController-driven timeout
 *   - Retry idempotent requests on transient failures (network/5xx)
 *     with exponential backoff + full jitter
 *   - Translate non-OK responses into a typed `ApiError`
 *
 * The wrapper deliberately stays low-level. Higher-level modules
 * (`runs.ts`, `events.ts`, etc.) call `apiFetch` and return typed data;
 * UI hooks consume those modules via TanStack Query.
 */
import { routerOrigin } from '@/lib/env'

export interface ApiFetchOptions extends Omit<RequestInit, 'signal'> {
  /** Request timeout in ms. Default 15s. SSE callers bypass this and
   *  manage their own AbortController. */
  timeoutMs?: number
  /** Total retry attempts on transient errors. Default 3. */
  retries?: number
  /** Override the Bearer token. Default: pull from Supabase session. */
  token?: string | null
  /** External AbortSignal that aborts the in-flight call. */
  signal?: AbortSignal
  /** Suppress automatic JSON parsing and return Response. Used by SSE
   *  + binary endpoints. */
  raw?: boolean
}

/** ApiError class — `instanceof ApiError` is intentional so callers can
 *  branch on it cleanly. Mirrors the `ApiError` interface in
 *  `lib/api/types.ts`; the class is the canonical runtime form. */
export class ApiError extends Error {
  status: number
  body?: unknown
  retryable: boolean
  requestId?: string

  constructor(
    status: number,
    message: string,
    body?: unknown,
    retryable = false,
    requestId?: string,
  ) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.body = body
    this.retryable = retryable
    this.requestId = requestId
  }
}

const RETRYABLE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS'])
const TRANSIENT_STATUSES = new Set([408, 425, 429, 500, 502, 503, 504])
const DEFAULT_TIMEOUT_MS = 15_000
const DEFAULT_RETRIES = 3

function jitterBackoff(attempt: number): number {
  // Full-jitter: random in [0, base * 2^attempt). Capped at 4s so the
  // user never waits more than ~8s for the whole retry budget.
  const base = 200
  const cap = 4_000
  const exp = Math.min(cap, base * Math.pow(2, attempt))
  return Math.floor(Math.random() * exp)
}

function makeRequestId(): string {
  // Cheap monotonic-ish id: epoch + small random tail. Good enough for
  // log correlation without dragging in a uuid dep.
  const r = Math.random().toString(36).slice(2, 10)
  return `c${Date.now().toString(36)}${r}`
}

function joinUrl(path: string): string {
  if (/^https?:\/\//.test(path)) return path
  if (path.startsWith('/api/')) return path
  const origin = routerOrigin()
  return `${origin}${path.startsWith('/') ? '' : '/'}${path}`
}

async function buildHeaders(
  init: ApiFetchOptions,
  requestId: string,
  resolveSessionToken: boolean,
): Promise<Headers> {
  const h = new Headers(init.headers)
  if (!h.has('accept')) h.set('accept', 'application/json')
  if (init.body && !h.has('content-type') && typeof init.body === 'string') {
    h.set('content-type', 'application/json')
  }
  h.set('x-request-id', requestId)
  // Token: explicit override > current Supabase session > none.
  let token: string | null | undefined = init.token
  if (token === undefined && resolveSessionToken) {
    const { getAccessToken } = await import('@/lib/auth/session')
    token = await getAccessToken()
  }
  if (token) {
    h.set('authorization', `Bearer ${token}`)
  }
  return h
}

/** Core fetch wrapper. Returns the parsed JSON body for success, or
 *  throws an `ApiError`. Retries idempotent requests on transient errors
 *  (network failures + the 5xx-ish status set above). */
export async function apiFetch<T = unknown>(path: string, init: ApiFetchOptions = {}): Promise<T> {
  const url = joinUrl(path)
  const resolveSessionToken = !url.startsWith('/')
  const method = (init.method ?? 'GET').toUpperCase()
  const retries = Math.max(0, init.retries ?? (RETRYABLE_METHODS.has(method) ? DEFAULT_RETRIES : 0))
  const timeout = init.timeoutMs ?? DEFAULT_TIMEOUT_MS
  const requestId = makeRequestId()

  let lastErr: unknown = null
  for (let attempt = 0; attempt <= retries; attempt++) {
    const ctrl = new AbortController()
    const onExternalAbort = () => ctrl.abort(init.signal?.reason)
    init.signal?.addEventListener('abort', onExternalAbort, { once: true })
    const timer = setTimeout(() => ctrl.abort(new DOMException('timeout', 'TimeoutError')), timeout)
    try {
      const headers = await buildHeaders(init, requestId, resolveSessionToken)
      const res = await fetch(url, {
        ...init,
        method,
        headers,
        signal: ctrl.signal,
        // Default to no-store for the JSON API; the daemon emits
        // Cache-Control where caching is safe (e.g. /version).
        cache: init.cache ?? 'no-store',
        credentials: init.credentials ?? 'omit',
      })

      if (init.raw) {
        if (!res.ok) {
          const body = await safeReadText(res)
          throw new ApiError(
            res.status,
            `${method} ${path}: ${res.status}`,
            body,
            isTransient(res.status),
            requestId,
          )
        }
        return res as unknown as T
      }

      if (!res.ok) {
        const body = await safeReadJson(res)
        const message = extractErrorMessage(body) ?? `${method} ${path}: ${res.status}`
        const err = new ApiError(res.status, message, body, isTransient(res.status), requestId)
        if (err.retryable && attempt < retries) {
          await sleep(jitterBackoff(attempt))
          continue
        }
        throw err
      }

      // 204 / empty body — return a typed null rather than parsing.
      if (res.status === 204 || res.headers.get('content-length') === '0') {
        return undefined as unknown as T
      }
      const ct = res.headers.get('content-type') ?? ''
      if (ct.includes('application/json')) {
        return (await res.json()) as T
      }
      // Fallback: text body. The daemon serves text on a few admin
      // endpoints; the caller can interpret it.
      return (await res.text()) as unknown as T
    } catch (e: unknown) {
      lastErr = e
      // AbortError + TimeoutError are wrapped into ApiError so callers
      // get a uniform branch.
      if (isAbortError(e)) {
        const reason =
          (e as { name?: string }).name === 'TimeoutError' ? 'request timed out' : 'request aborted'
        const isTimeout = (e as { name?: string }).name === 'TimeoutError'
        if (isTimeout && attempt < retries && RETRYABLE_METHODS.has(method)) {
          await sleep(jitterBackoff(attempt))
          continue
        }
        throw new ApiError(isTimeout ? 504 : 499, reason, undefined, isTimeout, requestId)
      }
      if (e instanceof ApiError) {
        throw e
      }
      // Network / DNS / TLS failure — transient, retry on idempotent.
      if (attempt < retries && RETRYABLE_METHODS.has(method)) {
        await sleep(jitterBackoff(attempt))
        continue
      }
      const msg = e instanceof Error ? e.message : String(e)
      throw new ApiError(0, `${method} ${path}: ${msg}`, undefined, true, requestId)
    } finally {
      clearTimeout(timer)
      init.signal?.removeEventListener('abort', onExternalAbort)
    }
  }
  // Unreachable — the loop either returns or throws — but the compiler
  // cannot prove it. Throw the last error to keep the type narrow.
  throw lastErr instanceof Error ? lastErr : new ApiError(0, 'apiFetch: exhausted retries')
}

function isTransient(status: number): boolean {
  return TRANSIENT_STATUSES.has(status)
}

function isAbortError(e: unknown): boolean {
  if (!e || typeof e !== 'object') return false
  const name = (e as { name?: string }).name
  return name === 'AbortError' || name === 'TimeoutError'
}

async function safeReadJson(res: Response): Promise<unknown> {
  try {
    const ct = res.headers.get('content-type') ?? ''
    if (ct.includes('application/json')) return await res.json()
    return await res.text()
  } catch {
    return null
  }
}

async function safeReadText(res: Response): Promise<string> {
  try {
    return await res.text()
  } catch {
    return ''
  }
}

function extractErrorMessage(body: unknown): string | null {
  if (!body || typeof body !== 'object') return null
  const m =
    (body as { error?: unknown; message?: unknown }).error ??
    (body as { message?: unknown }).message
  return typeof m === 'string' ? m : null
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms))
}

/** Convenience JSON POST/PUT/PATCH helper. */
export async function apiSend<T>(
  path: string,
  body: unknown,
  init: ApiFetchOptions & { method?: 'POST' | 'PUT' | 'PATCH' | 'DELETE' } = {},
): Promise<T> {
  return apiFetch<T>(path, {
    ...init,
    method: init.method ?? 'POST',
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: {
      'content-type': 'application/json',
      ...(init.headers ?? {}),
    },
  })
}
